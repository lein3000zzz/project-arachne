package cache

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

var (
	ErrCacheMiss = errors.New("cache miss")
)

type RedisCachedStorage struct {
	Logger *zap.SugaredLogger
	Client *redis.Client
}

func NewRedisCache(addr, password string, db int, logger *zap.SugaredLogger) *RedisCachedStorage {
	rdb := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})

	if err := rdb.ConfigSet(context.Background(), "maxmemory", "512mb").Err(); err != nil {
		log.Fatalf("failed to set redis maxmemory: %v", err)
	}

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("failed to connect to redis: %v", err)
	}

	return &RedisCachedStorage{
		Client: rdb,
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
