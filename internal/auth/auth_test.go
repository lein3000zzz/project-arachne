package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// fakeStore implements what a test touches; anything else panics on the nil
// embedded interface.
type fakeStore struct {
	Store

	accounts map[string]Account
	locked   time.Duration
	failures map[string]int
	locks    map[string]time.Duration
	cleared  []string
	rehashed string
	tokens   map[string][]byte
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		accounts: map[string]Account{},
		failures: map[string]int{},
		locks:    map[string]time.Duration{},
		tokens:   map[string][]byte{},
	}
}

func (f *fakeStore) AccountByName(_ context.Context, username string) (Account, error) {
	a, ok := f.accounts[strings.ToLower(username)]
	if !ok {
		return Account{}, ErrUserNotFound
	}

	return a, nil
}

func (f *fakeStore) LockedFor(context.Context, []string) (time.Duration, error) { return f.locked, nil }

func (f *fakeStore) RecordFailure(_ context.Context, key string, _ time.Duration) (int, error) {
	f.failures[key]++
	return f.failures[key], nil
}

func (f *fakeStore) Lock(_ context.Context, key string, d time.Duration) error {
	f.locks[key] = d
	return nil
}

func (f *fakeStore) ClearFailures(_ context.Context, key string) error {
	f.cleared = append(f.cleared, key)
	return nil
}

func (f *fakeStore) SetPasswordHash(_ context.Context, _ int64, hash string) error {
	f.rehashed = hash
	return nil
}

func (f *fakeStore) CreateSession(context.Context, []byte, int64, time.Duration, string) (time.Time, error) {
	return time.Now().Add(time.Hour), nil
}

func (f *fakeStore) SaveToken(_ context.Context, _ int64, name string, tokenHash []byte) error {
	f.tokens[name] = tokenHash
	return nil
}

// countingPasswords records verifications, which is what makes a login's
// timing the same for unknown users and wrong passwords.
type countingPasswords struct {
	*Hasher
	verified int
}

func (c *countingPasswords) Verify(ctx context.Context, encoded, password string) (bool, bool, error) {
	c.verified++
	return c.Hasher.Verify(ctx, encoded, password)
}

func newTestService(t *testing.T, store Store, params Params) (*Service, *countingPasswords) {
	t.Helper()

	hasher := NewHasher(params, 2)

	s, err := NewService(context.Background(), zap.NewNop().Sugar(), store, hasher, Config{
		SessionTTL: time.Hour, UserFailures: 3, IPFailures: 10,
		FailureWindow: time.Hour, Lockout: time.Minute, MaxLockout: 15 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	counting := &countingPasswords{Hasher: hasher}
	s.passwords = counting

	return s, counting
}

func addAccount(t *testing.T, store *fakeStore, h *Hasher, name, password string) {
	t.Helper()

	encoded, err := h.Hash(context.Background(), password)
	if err != nil {
		t.Fatal(err)
	}

	store.accounts[strings.ToLower(name)] = Account{User: User{ID: int64(len(store.accounts) + 1), Username: name}, PasswordHash: encoded}
}

func TestUnknownUserAndWrongPasswordLookTheSame(t *testing.T) {
	store := newFakeStore()
	s, counting := newTestService(t, store, cheap)
	addAccount(t, store, NewHasher(cheap, 1), "lein", "correct horse battery")

	_, unknown := s.Login(context.Background(), "nobody", "correct horse battery", "10.0.0.1", "")
	_, wrong := s.Login(context.Background(), "lein", "wrong horse battery", "10.0.0.1", "")

	if unknown == nil || wrong == nil || unknown.Error() != wrong.Error() || !errors.Is(unknown, ErrInvalidCredentials) {
		t.Errorf("unknown user: %v; wrong password: %v", unknown, wrong)
	}

	if counting.verified != 2 {
		t.Errorf("%d hashes checked for two logins: an unknown user must cost one too", counting.verified)
	}

	if store.failures["user:nobody"] != 1 || store.failures["user:lein"] != 1 || store.failures["ip:10.0.0.1"] != 2 {
		t.Errorf("failures = %v", store.failures)
	}
}

func TestDisabledUserCannotLogIn(t *testing.T) {
	store := newFakeStore()
	s, _ := newTestService(t, store, cheap)
	addAccount(t, store, NewHasher(cheap, 1), "lein", "correct horse battery")

	a := store.accounts["lein"]
	a.Disabled = true
	store.accounts["lein"] = a

	if _, err := s.Login(context.Background(), "lein", "correct horse battery", "", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("err = %v", err)
	}
}

func TestLockedLoginIsRefusedBeforeHashing(t *testing.T) {
	store := newFakeStore()
	store.locked = 90 * time.Second
	s, counting := newTestService(t, store, cheap)
	addAccount(t, store, NewHasher(cheap, 1), "lein", "correct horse battery")

	_, err := s.Login(context.Background(), "lein", "correct horse battery", "10.0.0.1", "")

	var throttled *ThrottledError
	if !errors.As(err, &throttled) || throttled.RetryAfter != 90*time.Second || !errors.Is(err, ErrThrottled) {
		t.Fatalf("err = %v", err)
	}

	if counting.verified != 0 {
		t.Error("a locked login still cost a hash")
	}
}

func TestFailuresPastTheLimitLock(t *testing.T) {
	store := newFakeStore()
	s, _ := newTestService(t, store, cheap)

	for range 5 {
		_, _ = s.Login(context.Background(), "lein", "wrong horse battery", "10.0.0.1", "")
	}

	if got := store.locks["user:lein"]; got != 2*time.Minute {
		t.Errorf("lock after the 5th failure with 3 tolerated = %s, want 2m", got)
	}

	if _, ok := store.locks["ip:10.0.0.1"]; ok {
		t.Error("the address was locked well below its limit")
	}
}

func TestLockoutDoublesUpToTheCeiling(t *testing.T) {
	cases := []struct {
		n    int
		want time.Duration
	}{
		{1, 0}, {5, 0}, {6, time.Minute}, {7, 2 * time.Minute}, {8, 4 * time.Minute},
		{10, 15 * time.Minute}, {50, 15 * time.Minute},
	}

	for _, tc := range cases {
		if got := lockout(tc.n, 5, time.Minute, 15*time.Minute); got != tc.want {
			t.Errorf("lockout(%d) = %s, want %s", tc.n, got, tc.want)
		}
	}
}

func TestLoginRehashesOldParametersAndClearsFailures(t *testing.T) {
	store := newFakeStore()
	stronger := cheap
	stronger.Time = 2
	s, _ := newTestService(t, store, stronger)
	addAccount(t, store, NewHasher(cheap, 1), "Lein", "correct horse battery")

	session, err := s.Login(context.Background(), "LEIN", "correct horse battery", "", "")
	if err != nil || session.Token == "" || session.User.Username != "Lein" {
		t.Fatalf("session = %+v, err = %v", session, err)
	}

	if !strings.Contains(store.rehashed, "t=2") {
		t.Errorf("re-hashed to %q, want the current parameters", store.rehashed)
	}

	if len(store.cleared) != 1 || store.cleared[0] != "user:lein" {
		t.Errorf("cleared = %v", store.cleared)
	}
}

func TestTokensAreStoredOnlyAsHashes(t *testing.T) {
	store := newFakeStore()
	s, _ := newTestService(t, store, cheap)
	addAccount(t, store, NewHasher(cheap, 1), "lein", "correct horse battery")

	token, err := s.CreateToken(context.Background(), "lein", "make")
	if err != nil || !strings.HasPrefix(token, tokenPrefix) || len(token) < 40 {
		t.Fatalf("token = %q, err = %v", token, err)
	}

	sum := sha256.Sum256([]byte(token))
	if !bytes.Equal(store.tokens["make"], sum[:]) {
		t.Error("the store did not get the token's sha256")
	}

	if _, err := s.CreateToken(context.Background(), "lein", "bad name"); !errors.Is(err, ErrInvalidTokenName) {
		t.Errorf("err = %v", err)
	}
}

func TestBearerTokensNeedThePrefix(t *testing.T) {
	s, _ := newTestService(t, newFakeStore(), cheap)

	for _, bad := range []string{"", "abc", strings.Repeat("a", 64), tokenPrefix + strings.Repeat("a", 200)} {
		if _, err := s.AuthenticateToken(context.Background(), bad); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

func TestAccountRules(t *testing.T) {
	s, _ := newTestService(t, newFakeStore(), cheap)
	ctx := context.Background()

	for _, name := range []string{"ab", "-lein", "lein with space", strings.Repeat("a", 33), "лейн"} {
		if _, err := s.CreateUser(ctx, name, "correct horse battery", false); !errors.Is(err, ErrInvalidUsername) {
			t.Errorf("username %q: err = %v", name, err)
		}
	}

	for _, password := range []string{"", "short", "elevenchars", strings.Repeat("x", maxPasswordBytes+1)} {
		if _, err := s.CreateUser(ctx, "lein", password, false); !errors.Is(err, ErrInvalidPassword) {
			t.Errorf("password of %d bytes: err = %v", len(password), err)
		}
	}

	// Twelve characters, more bytes: the rule counts characters.
	if err := validatePassword("пароль-длинн"); err != nil {
		t.Errorf("12 Cyrillic characters rejected: %v", err)
	}
}
