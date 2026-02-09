package cache

import (
	"context"
	"time"
)

type CachedStorage interface {
	Set(key string, value any, ttl time.Duration) error
	Get(key string) (string, error)
	Stop(ctx context.Context) error
}
