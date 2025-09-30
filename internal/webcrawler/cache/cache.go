package cache

import "time"

type CachedStorage interface {
	Set(key string, value any, ttl time.Duration) error
	Get(key string) (string, error)
}

const (
	BaseTTL = time.Hour * 12
)
