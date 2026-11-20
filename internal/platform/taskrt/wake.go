package taskrt

import (
	"context"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/redisx"
)

// WakeModule 是唤醒频道的模块段：频道名 le:<env>:task:task_wakeup:all（ADR-019 §8 / ADR-025 §1）。
const WakeModule = "task"

// Waker 在入队后唤醒 Worker。唤醒只是加速：失败/丢失都不影响正确性（Worker 按固定间隔轮询，ADR-019 §8）。
type Waker interface {
	Wake(ctx context.Context)
}

// RedisWaker 经 Redis Pub/Sub 唤醒；Redis 不可达时静默降级（只记调试日志），调用方不得据此回滚入队。
type RedisWaker struct {
	c   *redisx.Client
	ch  redisx.Channel
	log *zap.Logger
}

// NewRedisWaker 创建唤醒发布端。
func NewRedisWaker(c *redisx.Client, log *zap.Logger) (*RedisWaker, error) {
	ch, err := c.Keyspace().Channel(WakeModule, redisx.PurposeTaskWakeup)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &RedisWaker{c: c, ch: ch, log: log}, nil
}

// Channel 返回频道（订阅端使用）。
func (w *RedisWaker) Channel() redisx.Channel { return w.ch }

// Wake 实现 Waker。
func (w *RedisWaker) Wake(ctx context.Context) {
	if err := w.c.Publish(ctx, w.ch, "1"); err != nil {
		w.log.Debug("任务唤醒发布失败（已降级为轮询兜底）", zap.Error(err))
	}
}

// NopWaker 不做任何事（无 Redis 的装配与测试；Worker 靠轮询）。
type NopWaker struct{}

// Wake 实现 Waker。
func (NopWaker) Wake(context.Context) {}
