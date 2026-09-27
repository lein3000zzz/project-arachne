package runstates

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

const (
	slotsKey              = "crawler:run_slots"
	activeTasksKeyPrefix  = "run:active_tasks:"
	currentLinksKeyPrefix = "run:current_links:"
	tasksKeyPrefix        = "run:tasks:"
)

// Lease times are read from Redis's clock (TIME), not the node's, so clock skew
// between nodes cannot expire a live run early.
const nowMillis = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
`

var acquireSlotScript = redis.NewScript(nowMillis + `
local reaped = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', now)
if #reaped > 0 then
  redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
end

local expiry = now + tonumber(ARGV[3])
if redis.call('ZSCORE', KEYS[1], ARGV[1]) or redis.call('ZCARD', KEYS[1]) < tonumber(ARGV[2]) then
  redis.call('ZADD', KEYS[1], expiry, ARGV[1])
  return {1, reaped}
end

return {0, reaped}
`)

var admitTaskScript = redis.NewScript(nowMillis + `
local lease = redis.call('ZSCORE', KEYS[1], ARGV[1])
if not lease or tonumber(lease) <= now then
  return 3
end

if redis.call('HEXISTS', KEYS[2], ARGV[2]) == 1 then
  return 1
end

if tonumber(redis.call('GET', KEYS[4]) or '0') >= tonumber(ARGV[3]) then
  return 2
end

redis.call('HSET', KEYS[2], ARGV[2], 'queued')
redis.call('INCR', KEYS[3])
redis.call('INCR', KEYS[4])
for i = 2, 4 do
  redis.call('EXPIRE', KEYS[i], ARGV[4])
end

return 0
`)

var unadmitTaskScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], ARGV[1]) ~= 'queued' then
  return 0
end

redis.call('HDEL', KEYS[1], ARGV[1])
redis.call('DECR', KEYS[2])
redis.call('DECR', KEYS[3])

return 1
`)

var taskStateScript = redis.NewScript(nowMillis + `
local lease = redis.call('ZSCORE', KEYS[1], ARGV[1])
local active = lease and tonumber(lease) > now
if active then
  redis.call('ZADD', KEYS[1], 'XX', now + tonumber(ARGV[3]), ARGV[1])
end

local done = redis.call('HGET', KEYS[2], ARGV[2]) == 'done'

return {active and 1 or 0, done and 1 or 0}
`)

var completeTaskScript = redis.NewScript(nowMillis + `
if redis.call('HGET', KEYS[2], ARGV[2]) ~= 'queued' then
  return {0, 0}
end

redis.call('HSET', KEYS[2], ARGV[2], 'done')
local left = redis.call('DECR', KEYS[3])

local lease = redis.call('ZSCORE', KEYS[1], ARGV[1])
if lease and tonumber(lease) > now then
  redis.call('ZADD', KEYS[1], 'XX', now + tonumber(ARGV[3]), ARGV[1])
end

return {1, left}
`)

type RedisRunStateManager struct {
	client *redis.Client
	logger *zap.SugaredLogger
	nodeID string
	ttl    time.Duration
}

func NewRedisRunStateManager(client *redis.Client, logger *zap.SugaredLogger, nodeID string, ttl time.Duration) *RedisRunStateManager {
	return &RedisRunStateManager{
		client: client,
		logger: logger,
		nodeID: nodeID,
		ttl:    ttl,
	}
}

func (m *RedisRunStateManager) AcquireRunSlot(ctx context.Context, runID string, maxConcurrent int, lease time.Duration) (bool, []string, error) {
	raw, err := acquireSlotScript.Run(ctx, m.client, []string{slotsKey}, runID, maxConcurrent, lease.Milliseconds()).Slice()
	if err != nil {
		return false, nil, fmt.Errorf("acquiring run slot: %w", err)
	}

	if len(raw) != 2 {
		return false, nil, fmt.Errorf("acquiring run slot: unexpected reply %v", raw)
	}

	acquired, _ := raw[0].(int64)

	reaped := make([]string, 0)
	if list, ok := raw[1].([]any); ok {
		for _, id := range list {
			if s, ok := id.(string); ok {
				reaped = append(reaped, s)
			}
		}
	}

	for _, id := range reaped {
		m.logger.Warnw("Run lost its slot: no progress within its lease", "runID", id)
	}

	if acquired == 1 {
		m.logger.Infow("Acquired run slot", "runID", runID, "nodeID", m.nodeID)
	}

	return acquired == 1, reaped, nil
}

func (m *RedisRunStateManager) ReleaseRunSlot(ctx context.Context, runID string) error {
	if err := m.client.ZRem(ctx, slotsKey, runID).Err(); err != nil {
		return fmt.Errorf("releasing run slot: %w", err)
	}

	m.logger.Infow("Released run slot", "runID", runID, "nodeID", m.nodeID)

	return nil
}

func (m *RedisRunStateManager) AdmitTask(ctx context.Context, runID, taskID string, maxLinks int) (Admission, error) {
	keys := []string{slotsKey, tasksKeyPrefix + runID, activeTasksKeyPrefix + runID, currentLinksKeyPrefix + runID}

	result, err := admitTaskScript.Run(ctx, m.client, keys, runID, taskID, maxLinks, int64(m.ttl.Seconds())).Int()
	if err != nil {
		return 0, fmt.Errorf("admitting task: %w", err)
	}

	return Admission(result), nil
}

func (m *RedisRunStateManager) UnadmitTask(ctx context.Context, runID, taskID string) error {
	keys := []string{tasksKeyPrefix + runID, activeTasksKeyPrefix + runID, currentLinksKeyPrefix + runID}

	if err := unadmitTaskScript.Run(ctx, m.client, keys, taskID).Err(); err != nil {
		return fmt.Errorf("unadmitting task: %w", err)
	}

	return nil
}

func (m *RedisRunStateManager) TaskState(ctx context.Context, runID, taskID string, lease time.Duration) (TaskState, error) {
	keys := []string{slotsKey, tasksKeyPrefix + runID}

	raw, err := taskStateScript.Run(ctx, m.client, keys, runID, taskID, lease.Milliseconds()).Int64Slice()
	if err != nil {
		return TaskState{}, fmt.Errorf("reading task state: %w", err)
	}

	return TaskState{RunActive: raw[0] == 1, Done: raw[1] == 1}, nil
}

func (m *RedisRunStateManager) CompleteTask(ctx context.Context, runID, taskID string, lease time.Duration) (int64, bool, error) {
	keys := []string{slotsKey, tasksKeyPrefix + runID, activeTasksKeyPrefix + runID}

	raw, err := completeTaskScript.Run(ctx, m.client, keys, runID, taskID, lease.Milliseconds()).Int64Slice()
	if err != nil {
		return 0, false, fmt.Errorf("completing task: %w", err)
	}

	return raw[1], raw[0] == 1, nil
}

func (m *RedisRunStateManager) CleanupRun(ctx context.Context, runID string) error {
	keys := []string{activeTasksKeyPrefix + runID, currentLinksKeyPrefix + runID, tasksKeyPrefix + runID}

	if err := m.client.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("cleaning up run state: %w", err)
	}

	m.logger.Infow("Cleaned up run state", "runID", runID)

	return nil
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
