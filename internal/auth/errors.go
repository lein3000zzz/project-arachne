package auth

import (
	"errors"
	"fmt"
	"time"
)

var (
	// One error for an unknown user, a disabled one and a wrong password, so a
	// login cannot be used to find out which accounts exist.
	ErrInvalidCredentials = errors.New("wrong username or password")
	ErrThrottled          = errors.New("too many failed attempts")
	ErrUnauthenticated    = errors.New("not signed in")
	ErrInvalidUsername    = errors.New("a username is 3 to 32 letters, digits, '.', '_' or '-', starting with a letter or digit")
	ErrInvalidPassword    = fmt.Errorf("a password is at least %d characters and at most %d bytes", minPasswordRunes, maxPasswordBytes)
	ErrInvalidTokenName   = errors.New("a token name is 1 to 64 letters, digits, '.', '_' or '-'")
	ErrUserExists         = errors.New("that username is taken")
	ErrUserNotFound       = errors.New("no such user")
	ErrTokenNotFound      = errors.New("no such token")
	ErrMalformedHash      = errors.New("malformed password hash")
)

type ThrottledError struct {
	RetryAfter time.Duration
}

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("%s; try again in %s", ErrThrottled, e.RetryAfter.Round(time.Second))
}

func (e *ThrottledError) Unwrap() error { return ErrThrottled }
