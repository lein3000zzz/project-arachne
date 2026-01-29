package cache

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

type RedisCachedStorage struct {
	Logger *zap.SugaredLogger
	Client *redis.Client
}

func NewRedisCache(client *redis.Client, logger *zap.SugaredLogger) *RedisCachedStorage {
	return &RedisCachedStorage{
		Client: client,
		Logger: logger,
	}
}

func (c *RedisCachedStorage) Set(key string, value any, ttl time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return c.Client.Set(ctx, key, value, ttl).Err()
}

func (c *RedisCachedStorage) Get(key string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	val, err := c.Client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrCacheMiss
	}

	return val, err
}
