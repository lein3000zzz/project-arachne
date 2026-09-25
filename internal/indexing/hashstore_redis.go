package indexing

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	documentHashKeyPrefix = "doc:hash:"
	redisOperationTimeout = 5 * time.Second
)

type RedisHashStore struct {
	client *redis.Client
	ttl    time.Duration
}

func NewRedisHashStore(client *redis.Client, ttl time.Duration) *RedisHashStore {
	return &RedisHashStore{client: client, ttl: ttl}
}

func (s *RedisHashStore) Get(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, redisOperationTimeout)
	defer cancel()

	hash, err := s.client.Get(ctx, documentHashKeyPrefix+url).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrHashNotFound
	}

	return hash, err
}

func (s *RedisHashStore) Set(ctx context.Context, url, hash string) error {
	ctx, cancel := context.WithTimeout(ctx, redisOperationTimeout)
	defer cancel()

	return s.client.Set(ctx, documentHashKeyPrefix+url, hash, s.ttl).Err()
}

func (s *RedisHashStore) Shutdown(ctx context.Context) error {
	done := make(chan error, 1)
	go func() {
		done <- s.client.Close()
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
