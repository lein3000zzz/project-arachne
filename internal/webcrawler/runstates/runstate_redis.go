package runstates

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

const (
	activeTasksKeyPrefix       = "run:active_tasks:"
	currentLinksKeyPrefix      = "run:current_links:"
	runCompletionLockKeyPrefix = "run:completion_lock:"
	runSemaphoreKey            = "crawler:run_semaphore"

	DefaultRunStateTTL = 24 * time.Hour
	LockTTL            = 30 * time.Second
)

type RedisRunStateManager struct {
	client *redis.Client
	logger *zap.SugaredLogger
	nodeID string
}

func NewRedisRunStateManager(client *redis.Client, logger *zap.SugaredLogger, nodeID string) *RedisRunStateManager {
	return &RedisRunStateManager{
		client: client,
		logger: logger,
		nodeID: nodeID,
	}
}

func (m *RedisRunStateManager) activeTasksKey(runID string) string {
	return activeTasksKeyPrefix + runID
}

func (m *RedisRunStateManager) currentLinksKey(runID string) string {
	return currentLinksKeyPrefix + runID
}

func (m *RedisRunStateManager) completionLockKey(runID string) string {
	return runCompletionLockKeyPrefix + runID
}

func (m *RedisRunStateManager) IncrementActiveTasks(ctx context.Context, runID string) (int64, error) {
	key := m.activeTasksKey(runID)
	val, err := m.client.Incr(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("failed to increment active tasks: %w", err)
	}

	if val == 1 {
		m.client.Expire(ctx, key, DefaultRunStateTTL)
	}

	return val, nil
}

func (m *RedisRunStateManager) DecrementActiveTasks(ctx context.Context, runID string) (int64, error) {
	key := m.activeTasksKey(runID)
	val, err := m.client.Decr(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("failed to decrement active tasks: %w", err)
	}
	return val, nil
}

func (m *RedisRunStateManager) IncrementCurrentLinks(ctx context.Context, runID string) (int64, error) {
	key := m.currentLinksKey(runID)
	val, err := m.client.Incr(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("failed to increment current links: %w", err)
	}

	if val == 1 {
		m.client.Expire(ctx, key, DefaultRunStateTTL)
	}

	return val, nil
}

func (m *RedisRunStateManager) GetCurrentLinks(ctx context.Context, runID string) (int64, error) {
	key := m.currentLinksKey(runID)

	val, err := m.client.Get(ctx, key).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("failed to get current links: %w", err)
	}
	return val, nil
}

func (m *RedisRunStateManager) GetActiveTasks(ctx context.Context, runID string) (int64, error) {
	key := m.activeTasksKey(runID)

	val, err := m.client.Get(ctx, key).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("failed to get active tasks: %w", err)
	}
	return val, nil
}

func (m *RedisRunStateManager) IncrementActiveAndCurrentLinks(ctx context.Context, runID string) error {
	pipe := m.client.TxPipeline()

	activeKey := m.activeTasksKey(runID)
	linksKey := m.currentLinksKey(runID)

	pipe.Incr(ctx, activeKey)
	pipe.Incr(ctx, linksKey)
	pipe.Expire(ctx, activeKey, DefaultRunStateTTL)
	pipe.Expire(ctx, linksKey, DefaultRunStateTTL)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to increment active and current links: %w", err)
	}

	return nil
}

func (m *RedisRunStateManager) AcquireRunCompletionLock(ctx context.Context, runID string, ttl time.Duration) (bool, error) {
	key := m.completionLockKey(runID)

	acquired, err := m.client.SetNX(ctx, key, m.nodeID, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("failed to acquire completion lock: %w", err)
	}

	if acquired {
		m.logger.Debugw("Acquired run completion lock", "runID", runID, "nodeID", m.nodeID)
	}

	return acquired, nil
}

func (m *RedisRunStateManager) ReleaseRunCompletionLock(ctx context.Context, runID string) error {
	key := m.completionLockKey(runID)

	script := redis.NewScript(`
		if redis.call("get", KEYS[1]) == ARGV[1] then
			return redis.call("del", KEYS[1])
		else
			return 0
		end
	`)

	_, err := script.Run(ctx, m.client, []string{key}, m.nodeID).Result()
	if err != nil {
		return fmt.Errorf("failed to release completion lock: %w", err)
	}

	return nil
}

func (m *RedisRunStateManager) AcquireRunSlot(ctx context.Context, maxConcurrent int) (bool, error) {
	script := redis.NewScript(`
		local current = redis.call("get", KEYS[1])
		if current == false then
			current = 0
		else
			current = tonumber(current)
		end
		
		if current < tonumber(ARGV[1]) then
			redis.call("incr", KEYS[1])
			return 1
		else
			return 0
		end
	`)

	result, err := script.Run(ctx, m.client, []string{runSemaphoreKey}, maxConcurrent).Int64()
	if err != nil {
		return false, fmt.Errorf("failed to acquire run slot: %w", err)
	}

	acquired := result == 1
	if acquired {
		m.logger.Infow("Acquired run slot", "nodeID", m.nodeID)
	}

	return acquired, nil
}

func (m *RedisRunStateManager) ReleaseRunSlot(ctx context.Context) error {
	script := redis.NewScript(`
		local current = redis.call("get", KEYS[1])
		if current == false or tonumber(current) <= 0 then
			return 0
		else
			return redis.call("decr", KEYS[1])
		end
	`)

	_, err := script.Run(ctx, m.client, []string{runSemaphoreKey}).Result()
	if err != nil {
		return fmt.Errorf("failed to release run slot: %w", err)
	}

	m.logger.Infow("Released run slot", "nodeID", m.nodeID)
	return nil
}

func (m *RedisRunStateManager) CleanupRun(ctx context.Context, runID string) error {
	keys := []string{
		m.activeTasksKey(runID),
		m.currentLinksKey(runID),
		m.completionLockKey(runID),
	}

	err := m.client.Del(ctx, keys...).Err()
	if err != nil {
		return fmt.Errorf("failed to cleanup run state: %w", err)
	}

	m.logger.Infow("Cleaned up run state", "runID", runID)
	return nil
}

func (m *RedisRunStateManager) SetRunTTL(ctx context.Context, runID string, ttl time.Duration) error {
	pipe := m.client.TxPipeline()

	pipe.Expire(ctx, m.activeTasksKey(runID), ttl)
	pipe.Expire(ctx, m.currentLinksKey(runID), ttl)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to set run TTL: %w", err)
	}

	return nil
}

func (m *RedisRunStateManager) CheckRunLimits(ctx context.Context, runID string, maxLinks int) (bool, error) {
	currentLinks, err := m.GetCurrentLinks(ctx, runID)
	if err != nil {
		return false, err
	}

	return currentLinks < int64(maxLinks), nil
}

func (m *RedisRunStateManager) Stop(ctx context.Context) error {
	done := make(chan error, 1)
	go func() {
		done <- m.client.Close()
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
