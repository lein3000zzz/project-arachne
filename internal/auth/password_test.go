package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

var cheap = Params{Time: 1, Memory: 1024, Threads: 1, SaltLen: 16, KeyLen: 32}

func TestHashVerifiesOnlyItsPassword(t *testing.T) {
	ctx := context.Background()
	h := NewHasher(cheap, 2)

	a, err := h.Hash(ctx, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}

	b, _ := h.Hash(ctx, "correct horse battery")
	if a == b {
		t.Error("two hashes of one password are equal: the salt is not random")
	}

	if !strings.HasPrefix(a, "$argon2id$v=19$m=1024,t=1,p=1$") {
		t.Errorf("not a PHC string: %s", a)
	}

	if ok, stale, err := h.Verify(ctx, a, "correct horse battery"); !ok || stale || err != nil {
		t.Errorf("right password: ok=%v stale=%v err=%v", ok, stale, err)
	}

	if ok, _, err := h.Verify(ctx, a, "correct horse batterY"); ok || err != nil {
		t.Errorf("wrong password: ok=%v err=%v", ok, err)
	}
}

func TestVerifyFlagsHashesWithOldParameters(t *testing.T) {
	ctx := context.Background()

	old, err := NewHasher(cheap, 1).Hash(ctx, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}

	stronger := cheap
	stronger.Time = 2

	ok, stale, err := NewHasher(stronger, 1).Verify(ctx, old, "correct horse battery")
	if !ok || !stale || err != nil {
		t.Errorf("ok=%v stale=%v err=%v, want a match that asks for a re-hash", ok, stale, err)
	}
}

func TestDefaultParamsAreRecorded(t *testing.T) {
	encoded, err := NewHasher(DefaultParams, 1).Hash(context.Background(), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Errorf("hash = %s", encoded)
	}
}

func TestMalformedHashesAreRejectedNotPanicking(t *testing.T) {
	h := NewHasher(cheap, 1)

	for _, bad := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=1024,t=1,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=16$m=1024,t=1,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=1024,t=0,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=1024,t=1,p=0$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=1024,t=1,p=1$!!!$a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=1024,t=1,p=1$c2FsdHNhbHRzYWx0$a2V5",
	} {
		if _, _, err := h.Verify(context.Background(), bad, "x"); !errors.Is(err, ErrMalformedHash) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

func TestHashingWaitsForASlotOrTheContext(t *testing.T) {
	h := NewHasher(cheap, 1)
	h.slots <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := h.Hash(ctx, "correct horse battery"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the context's", err)
	}
}
