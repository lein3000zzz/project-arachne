package endpoints

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

var errSeal = errors.New("a saved key could not be opened: ENDPOINT_KEY changed since it was saved")

// sealer encrypts users' API keys with AES-256-GCM. The user's id is bound in
// as additional data: a sealed key copied to another user's row does not open.
type sealer struct {
	aead cipher.AEAD
}

func newSealer(key []byte) (*sealer, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("%w: the key is %d bytes, not 32", ErrUnavailable, len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	return &sealer{aead: aead}, nil
}

func (s *sealer) seal(plain, userID string) []byte {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}

	return s.aead.Seal(nonce, nonce, []byte(plain), bound(userID))
}

func (s *sealer) open(sealed []byte, userID string) (string, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return "", errSeal
	}

	plain, err := s.aead.Open(nil, sealed[:n], sealed[n:], bound(userID))
	if err != nil {
		return "", errSeal
	}

	return string(plain), nil
}

func bound(userID string) []byte {
	return []byte("arachne endpoint key, user " + userID)
}
