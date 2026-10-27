//go:build integration

package auth

import (
	"context"
	"errors"
	"testing"
	"time"
	"web-crawler/internal/database/dbtest"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

func newPostgresService(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()

	pool := dbtest.NewPool(t)

	s, err := NewService(context.Background(), zap.NewNop().Sugar(), NewPostgresStore(pool), NewHasher(cheap, 2), Config{
		SessionTTL: 24 * time.Hour, UserFailures: 2, IPFailures: 100,
		FailureWindow: time.Hour, Lockout: time.Minute, MaxLockout: 15 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	return s, pool
}

func TestUsersAndSessions(t *testing.T) {
	s, pool := newPostgresService(t)
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, "Lein", "correct horse battery", true); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateUser(ctx, "lein", "another long password", false); !errors.Is(err, ErrUserExists) {
		t.Errorf("same name in another case: %v", err)
	}

	session, err := s.Login(ctx, "lein", "correct horse battery", "10.0.0.1", "test-agent")
	if err != nil || !session.User.Admin || session.User.Username != "Lein" {
		t.Fatalf("login: %+v, %v", session, err)
	}

	user, renewed, err := s.Authenticate(ctx, session.Token)
	if err != nil || user.ID != session.User.ID || !renewed.IsZero() {
		t.Fatalf("fresh session: %+v renewed=%v err=%v", user, renewed, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE sessions SET last_seen_at = now() - interval '2 hours', expires_at = now() + interval '1 minute'`); err != nil {
		t.Fatal(err)
	}

	if _, renewed, err := s.Authenticate(ctx, session.Token); err != nil || time.Until(renewed) < 23*time.Hour {
		t.Errorf("a session in use is renewed: until %v, err %v", renewed, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE sessions SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Authenticate(ctx, session.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("expired session: %v", err)
	}

	second, err := s.Login(ctx, "lein", "correct horse battery", "", "")
	if err != nil {
		t.Fatal(err)
	}

	var stored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&stored); err != nil || stored != 1 {
		t.Errorf("%d sessions stored, want only the live one: expired ones are swept at login", stored)
	}

	if err := s.Logout(ctx, second.Token); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Authenticate(ctx, second.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("after logout: %v", err)
	}
}

func TestFailedLoginsLockAndExpire(t *testing.T) {
	s, pool := newPostgresService(t)
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, "lein", "correct horse battery", false); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if _, err := s.Login(ctx, "lein", "wrong horse battery", "10.0.0.1", ""); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("wrong password: %v", err)
		}
	}

	_, err := s.Login(ctx, "lein", "correct horse battery", "10.0.0.2", "")

	var throttled *ThrottledError
	if !errors.As(err, &throttled) || throttled.RetryAfter <= 50*time.Second || throttled.RetryAfter > time.Minute {
		t.Fatalf("right password while locked: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE login_failures SET locked_until = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Login(ctx, "lein", "correct horse battery", "10.0.0.2", ""); err != nil {
		t.Fatalf("after the lock: %v", err)
	}

	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM login_failures WHERE key = 'user:lein'`).Scan(&left); err != nil || left != 0 {
		t.Errorf("a successful login leaves %d failure rows for the name", left)
	}
}

func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	s, _ := newPostgresService(t)
	ctx := context.Background()

	user, err := s.CreateUser(ctx, "lein", "correct horse battery", false)
	if err != nil {
		t.Fatal(err)
	}

	other, _ := s.Login(ctx, "lein", "correct horse battery", "", "")

	if _, err := s.ChangePassword(ctx, user, "wrong horse battery", "a brand new password", "", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("wrong current password: %v", err)
	}

	mine, err := s.ChangePassword(ctx, user, "correct horse battery", "a brand new password", "", "")
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Authenticate(ctx, other.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("another session survived the change: %v", err)
	}

	if _, _, err := s.Authenticate(ctx, mine.Token); err != nil {
		t.Errorf("the changer's new session: %v", err)
	}

	if _, err := s.Login(ctx, "lein", "a brand new password", "", ""); err != nil {
		t.Errorf("login with the new password: %v", err)
	}

	if err := s.SetPassword(ctx, "lein", "reset by the admin"); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Authenticate(ctx, mine.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a session survived the admin's reset: %v", err)
	}
}

func TestTokensRotateRevokeAndDieWithTheUser(t *testing.T) {
	s, _ := newPostgresService(t)
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, "lein", "correct horse battery", false); err != nil {
		t.Fatal(err)
	}

	first, err := s.CreateToken(ctx, "lein", "make")
	if err != nil {
		t.Fatal(err)
	}

	if user, err := s.AuthenticateToken(ctx, first); err != nil || user.Username != "lein" {
		t.Fatalf("token: %+v, %v", user, err)
	}

	second, err := s.CreateToken(ctx, "lein", "make")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.AuthenticateToken(ctx, first); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a replaced token still works: %v", err)
	}

	session, _ := s.Login(ctx, "lein", "correct horse battery", "", "")

	if err := s.SetDisabled(ctx, "lein", true); err != nil {
		t.Fatal(err)
	}

	if _, err := s.AuthenticateToken(ctx, second); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a disabled user's token: %v", err)
	}

	if _, _, err := s.Authenticate(ctx, session.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a disabled user's session: %v", err)
	}

	if _, err := s.Login(ctx, "lein", "correct horse battery", "", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("a disabled user's login: %v", err)
	}

	if err := s.SetDisabled(ctx, "lein", false); err != nil {
		t.Fatal(err)
	}

	if err := s.RevokeToken(ctx, "lein", "make"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.AuthenticateToken(ctx, second); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a revoked token: %v", err)
	}

	if err := s.RevokeToken(ctx, "lein", "make"); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("revoking twice: %v", err)
	}
}
