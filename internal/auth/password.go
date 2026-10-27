package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are argon2id's costs. Every hash records its own, so raising them
// affects new hashes only, and a login with an older hash re-hashes it.
type Params struct {
	Time    uint32
	Memory  uint32 // KiB
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// DefaultParams is RFC 9106's second recommendation, the one for machines that
// cannot spare 2 GiB per hash.
var DefaultParams = Params{Time: 3, Memory: 64 * 1024, Threads: 4, SaltLen: 16, KeyLen: 32}

type Hasher struct {
	params Params
	// Each hash holds Params.Memory while it runs, and a burst of logins must not
	// take the CPU from the models.
	slots chan struct{}
}

func NewHasher(params Params, concurrency int) *Hasher {
	return &Hasher{params: params, slots: make(chan struct{}, max(concurrency, 1))}
}

func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, h.params.SaltLen)
	_, _ = rand.Read(salt)

	key, err := h.derive(ctx, h.params, salt, password)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version,
		h.params.Memory, h.params.Time, h.params.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify reports whether password matches encoded, and whether encoded was made
// with other parameters than the current ones and should be replaced.
func (h *Hasher) Verify(ctx context.Context, encoded, password string) (ok, stale bool, err error) {
	params, salt, want, err := decode(encoded)
	if err != nil {
		return false, false, err
	}

	got, err := h.derive(ctx, params, salt, password)
	if err != nil {
		return false, false, err
	}

	return subtle.ConstantTimeCompare(got, want) == 1, params != h.params, nil
}

func (h *Hasher) derive(ctx context.Context, p Params, salt []byte, password string) ([]byte, error) {
	select {
	case h.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-h.slots }()

	return argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen), nil
}

func decode(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Params{}, nil, nil, ErrMalformedHash
	}

	var (
		p       Params
		version int
	)

	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return Params{}, nil, nil, fmt.Errorf("%w: version %q", ErrMalformedHash, parts[2])
	}

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return Params{}, nil, nil, fmt.Errorf("%w: parameters %q", ErrMalformedHash, parts[3])
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("%w: salt", ErrMalformedHash)
	}

	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("%w: key", ErrMalformedHash)
	}

	// argon2 panics on zero rounds or threads.
	if p.Time == 0 || p.Memory == 0 || p.Threads == 0 || len(salt) < 8 || len(key) < 16 {
		return Params{}, nil, nil, fmt.Errorf("%w: parameters out of range", ErrMalformedHash)
	}

	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(key))

	return p, salt, key, nil
}
