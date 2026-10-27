// Package auth keeps user accounts, their sessions and API tokens, and the
// throttle on failed logins.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"
)

type User struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	Admin     bool      `json:"admin"`
	CreatedAt time.Time `json:"created_at"`
}

type Account struct {
	User
	PasswordHash string
	Disabled     bool
}

type Session struct {
	Token     string
	User      User
	ExpiresAt time.Time
}

type Config struct {
	SessionTTL time.Duration
	// Failed logins tolerated per username and per client address within
	// FailureWindow; the next locks them for Lockout, doubling with every
	// further failure up to MaxLockout.
	UserFailures  int
	IPFailures    int
	FailureWindow time.Duration
	Lockout       time.Duration
	MaxLockout    time.Duration
}

type Store interface {
	UserCount(ctx context.Context) (int, error)
	CreateUser(ctx context.Context, username, passwordHash string, admin bool) (User, error)
	AccountByName(ctx context.Context, username string) (Account, error)
	PasswordHash(ctx context.Context, userID int64) (string, error)
	// SetPasswordHash keeps the user's sessions; ReplacePassword ends them.
	SetPasswordHash(ctx context.Context, userID int64, hash string) error
	ReplacePassword(ctx context.Context, userID int64, hash string) error
	SetDisabled(ctx context.Context, userID int64, disabled bool) error

	CreateSession(ctx context.Context, tokenHash []byte, userID int64, ttl time.Duration, userAgent string) (time.Time, error)
	// SessionUser renews a session last seen more than renewAfter ago and then
	// returns its new expiry; otherwise the zero time.
	SessionUser(ctx context.Context, tokenHash []byte, ttl, renewAfter time.Duration) (User, time.Time, error)
	DeleteSession(ctx context.Context, tokenHash []byte) error

	SaveToken(ctx context.Context, userID int64, name string, tokenHash []byte) error
	DeleteToken(ctx context.Context, userID int64, name string) error
	TokenUser(ctx context.Context, tokenHash []byte, touchAfter time.Duration) (User, error)

	LockedFor(ctx context.Context, keys []string) (time.Duration, error)
	RecordFailure(ctx context.Context, key string, window time.Duration) (int, error)
	Lock(ctx context.Context, key string, d time.Duration) error
	ClearFailures(ctx context.Context, key string) error
}

type passwords interface {
	Hash(ctx context.Context, password string) (string, error)
	Verify(ctx context.Context, encoded, password string) (ok, stale bool, err error)
}

const (
	minPasswordRunes = 12
	maxPasswordBytes = 1024
	// A session is written to at most this often, not on every request.
	renewAfter = time.Hour
	// Prefixed so secret scanners and people can tell what a leaked one is.
	tokenPrefix   = "arachne_"
	maxUserAgent  = 256
	maxTokenBytes = 128
)

var (
	usernamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{2,31}$`)
	tokenNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

type Service struct {
	logger    *zap.SugaredLogger
	store     Store
	passwords passwords
	cfg       Config
	// Checked against for unknown users, so they cost a login as much time as
	// a wrong password does.
	dummy string
}

func NewService(ctx context.Context, logger *zap.SugaredLogger, store Store, hasher *Hasher, cfg Config) (*Service, error) {
	dummy, err := hasher.Hash(ctx, "not anyone's password")
	if err != nil {
		return nil, err
	}

	return &Service{logger: logger, store: store, passwords: hasher, cfg: cfg, dummy: dummy}, nil
}

func (s *Service) Login(ctx context.Context, username, password, addr, userAgent string) (Session, error) {
	limits := s.limits(username, addr)

	if err := s.throttled(ctx, limits); err != nil {
		return Session{}, err
	}

	account := Account{PasswordHash: s.dummy}

	if usernamePattern.MatchString(username) {
		found, err := s.store.AccountByName(ctx, username)

		switch {
		case err == nil && !found.Disabled:
			account = found
		case err == nil, errors.Is(err, ErrUserNotFound):
		default:
			return Session{}, err
		}
	}

	ok, stale, err := s.passwords.Verify(ctx, account.PasswordHash, password)
	if err != nil {
		return Session{}, err
	}

	if !ok || account.ID == 0 {
		return Session{}, s.failed(ctx, limits)
	}

	if stale {
		s.rehash(ctx, account.ID, password)
	}

	s.clear(ctx, userKey(username))

	return s.startSession(ctx, account.User, userAgent)
}

func (s *Service) Logout(ctx context.Context, token string) error {
	return s.store.DeleteSession(ctx, hashToken(token))
}

// Authenticate returns the session's user, and its new expiry when this request
// renewed it.
func (s *Service) Authenticate(ctx context.Context, token string) (User, time.Time, error) {
	if token == "" || len(token) > maxTokenBytes {
		return User{}, time.Time{}, ErrUnauthenticated
	}

	return s.store.SessionUser(ctx, hashToken(token), s.cfg.SessionTTL, renewAfter)
}

func (s *Service) AuthenticateToken(ctx context.Context, token string) (User, error) {
	if !strings.HasPrefix(token, tokenPrefix) || len(token) > maxTokenBytes {
		return User{}, ErrUnauthenticated
	}

	return s.store.TokenUser(ctx, hashToken(token), renewAfter)
}

// ChangePassword ends every session of the user, then starts one for the caller.
func (s *Service) ChangePassword(ctx context.Context, user User, current, next, addr, userAgent string) (Session, error) {
	if err := validatePassword(next); err != nil {
		return Session{}, err
	}

	limits := s.limits(user.Username, addr)

	if err := s.throttled(ctx, limits); err != nil {
		return Session{}, err
	}

	encoded, err := s.store.PasswordHash(ctx, user.ID)
	if err != nil {
		return Session{}, err
	}

	ok, _, err := s.passwords.Verify(ctx, encoded, current)
	if err != nil {
		return Session{}, err
	}

	if !ok {
		return Session{}, s.failed(ctx, limits)
	}

	if err := s.replacePassword(ctx, user.ID, next); err != nil {
		return Session{}, err
	}

	return s.startSession(ctx, user, userAgent)
}

func (s *Service) UserCount(ctx context.Context) (int, error) {
	return s.store.UserCount(ctx)
}

func (s *Service) CreateUser(ctx context.Context, username, password string, admin bool) (User, error) {
	if !usernamePattern.MatchString(username) {
		return User{}, ErrInvalidUsername
	}

	if err := validatePassword(password); err != nil {
		return User{}, err
	}

	encoded, err := s.passwords.Hash(ctx, password)
	if err != nil {
		return User{}, err
	}

	return s.store.CreateUser(ctx, username, encoded, admin)
}

// SetPassword is the admin's reset: it ends the user's sessions and lifts a
// lock on their name.
func (s *Service) SetPassword(ctx context.Context, username, password string) error {
	if err := validatePassword(password); err != nil {
		return err
	}

	account, err := s.store.AccountByName(ctx, username)
	if err != nil {
		return err
	}

	if err := s.replacePassword(ctx, account.ID, password); err != nil {
		return err
	}

	return s.store.ClearFailures(ctx, userKey(username))
}

func (s *Service) SetDisabled(ctx context.Context, username string, disabled bool) error {
	account, err := s.store.AccountByName(ctx, username)
	if err != nil {
		return err
	}

	return s.store.SetDisabled(ctx, account.ID, disabled)
}

// CreateToken returns a new API token, the only time it is ever shown. One with
// the same name is replaced.
func (s *Service) CreateToken(ctx context.Context, username, name string) (string, error) {
	if !tokenNamePattern.MatchString(name) {
		return "", ErrInvalidTokenName
	}

	account, err := s.store.AccountByName(ctx, username)
	if err != nil {
		return "", err
	}

	token := newToken(tokenPrefix)

	if err := s.store.SaveToken(ctx, account.ID, name, hashToken(token)); err != nil {
		return "", err
	}

	return token, nil
}

func (s *Service) RevokeToken(ctx context.Context, username, name string) error {
	account, err := s.store.AccountByName(ctx, username)
	if err != nil {
		return err
	}

	return s.store.DeleteToken(ctx, account.ID, name)
}

func (s *Service) startSession(ctx context.Context, user User, userAgent string) (Session, error) {
	if len(userAgent) > maxUserAgent {
		userAgent = strings.ToValidUTF8(userAgent[:maxUserAgent], "")
	}

	token := newToken("")

	expires, err := s.store.CreateSession(ctx, hashToken(token), user.ID, s.cfg.SessionTTL, userAgent)
	if err != nil {
		return Session{}, err
	}

	return Session{Token: token, User: user, ExpiresAt: expires}, nil
}

func (s *Service) replacePassword(ctx context.Context, userID int64, password string) error {
	encoded, err := s.passwords.Hash(ctx, password)
	if err != nil {
		return err
	}

	return s.store.ReplacePassword(ctx, userID, encoded)
}

func (s *Service) rehash(ctx context.Context, userID int64, password string) {
	encoded, err := s.passwords.Hash(ctx, password)
	if err == nil {
		err = s.store.SetPasswordHash(ctx, userID, encoded)
	}

	if err != nil {
		s.logger.Warnw("Failed to re-hash a password with the current parameters", "userID", userID, "error", err)
	}
}

type limit struct {
	key string
	max int
}

func userKey(username string) string { return "user:" + strings.ToLower(username) }

// A name that cannot exist is not counted: it would only let anyone fill the
// table with junk keys.
func (s *Service) limits(username, addr string) []limit {
	var limits []limit

	if usernamePattern.MatchString(username) {
		limits = append(limits, limit{userKey(username), s.cfg.UserFailures})
	}

	if addr != "" {
		limits = append(limits, limit{"ip:" + addr, s.cfg.IPFailures})
	}

	return limits
}

func (s *Service) throttled(ctx context.Context, limits []limit) error {
	keys := make([]string, len(limits))
	for i, l := range limits {
		keys[i] = l.key
	}

	wait, err := s.store.LockedFor(ctx, keys)
	if err != nil {
		return err
	}

	if wait > 0 {
		return &ThrottledError{RetryAfter: wait}
	}

	return nil
}

func (s *Service) failed(ctx context.Context, limits []limit) error {
	for _, l := range limits {
		n, err := s.store.RecordFailure(ctx, l.key, s.cfg.FailureWindow)
		if err != nil {
			return err
		}

		if d := lockout(n, l.max, s.cfg.Lockout, s.cfg.MaxLockout); d > 0 {
			if err := s.store.Lock(ctx, l.key, d); err != nil {
				return err
			}
		}
	}

	return ErrInvalidCredentials
}

func (s *Service) clear(ctx context.Context, key string) {
	if err := s.store.ClearFailures(ctx, key); err != nil {
		s.logger.Warnw("Failed to clear failed logins", "key", key, "error", err)
	}
}

// lockout is how long the n-th failure in a row locks for: nothing while
// tolerated, then base, doubling with each further failure, up to ceiling.
func lockout(n, tolerated int, base, ceiling time.Duration) time.Duration {
	if n <= tolerated {
		return 0
	}

	d := base
	for i := tolerated + 1; i < n && d < ceiling; i++ {
		d *= 2
	}

	return min(d, ceiling)
}

func validatePassword(password string) error {
	if utf8.RuneCountInString(password) < minPasswordRunes || len(password) > maxPasswordBytes {
		return ErrInvalidPassword
	}

	return nil
}

func newToken(prefix string) string {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)

	return prefix + base64.RawURLEncoding.EncodeToString(secret)
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

type contextKey struct{}

func WithUser(ctx context.Context, user User) context.Context {
	return context.WithValue(ctx, contextKey{}, user)
}

func UserFrom(ctx context.Context) (User, bool) {
	user, ok := ctx.Value(contextKey{}).(User)
	return user, ok
}
