package redisx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/pkg/redact"
)

// Purpose 是 Pub/Sub 的用途（ADR-025 §1 穷尽清单）。清单之外的用途不能创建频道。
type Purpose string

const (
	PurposeCertRevoke  Purpose = "cert_revoke"  // 节点证书吊销广播（ADR-003 §6，兜底：短 TTL 轮询 MySQL）
	PurposePermVersion Purpose = "perm_version" // 权限版本失效（兜底：缓存短 TTL）
	PurposeTaskWakeup  Purpose = "task_wakeup"  // 任务唤醒（兜底：Worker 轮询，ADR-019）
	// PurposeSettingChange 系统设置变更的缓存失效广播（Story 1.22b；属 ADR-025 §1「失效广播/缓存清除的 Pub/Sub」）。
	// 兜底：设置快照 TTL（默认 5 秒）+ 订阅轮询对账（启动、周期、Redis 恢复时丢弃快照）。消息体不含任何设置值，只是「已变更」。
	PurposeSettingChange Purpose = "setting_change"
)

// 订阅/频道错误。
var (
	ErrBadPurpose = errors.New("redisx: 频道用途不在 ADR-025 清单内")
	ErrNoFallback = errors.New("redisx: 订阅必须提供轮询兜底（Poll 与 >0 的 PollInterval）：Pub/Sub 有损，不得假定消息必达")
)

// Channel 是 Pub/Sub 频道，命名同键空间 le:<env>:<模块>:<用途>:<标识>（Pub/Sub 无 TTL 概念，不受 TTL 约束）。
type Channel struct{ s string }

// String 返回频道名。
func (ch Channel) String() string { return ch.s }

// Channel 构造频道；id 缺省为 "all"。
func (ks Keyspace) Channel(module string, p Purpose, id ...string) (Channel, error) {
	switch p {
	case PurposeCertRevoke, PurposePermVersion, PurposeTaskWakeup, PurposeSettingChange:
	default:
		return Channel{}, fmt.Errorf("%w: %q", ErrBadPurpose, p)
	}
	if len(id) == 0 {
		id = []string{"all"}
	}
	k, err := ks.Key(module, string(p), id...)
	if err != nil {
		return Channel{}, err
	}
	return Channel(k), nil
}

// Publish 发布广播。有损：Redis 不可用时返回 ErrUnavailable，调用方应忽略（订阅者有轮询兜底），不得据此回滚业务。
func (c *Client) Publish(ctx context.Context, ch Channel, msg string) error {
	if ch.s == "" {
		return ErrBadKey
	}
	if redact.String(msg) != msg {
		return ErrSensitiveValue
	}
	return c.run(ctx, func(r *redis.Client) error { return r.Publish(ctx, ch.s, msg).Err() })
}

// SubscribeOptions 描述一个订阅。
type SubscribeOptions struct {
	// Handler 处理收到的消息。panic 会被捕获并记录。
	Handler func(ctx context.Context, msg string)
	// Poll 是轮询兜底：从真相源对账（如读 MySQL 的权限版本/吊销列表/待处理任务）。订阅启动时、每个 PollInterval、
	// 以及 Redis 从故障中恢复时都会调用——广播丢失（Redis 宕机、重启、订阅断线）最迟在一个 PollInterval 内被补上。
	// 注意：Handler 与 Poll 在同一协程顺序执行，慢 Handler 会推迟下一次轮询；耗时工作请自行异步化。
	// Redis 在两次健康探测之间（<ProbeInterval）极快重启时客户端状态不翻转，订阅断线期间丢失的广播由下一个 PollInterval 补上。
	Poll         func(ctx context.Context) error
	PollInterval time.Duration
}

// Subscription 是一个运行中的订阅。
type Subscription struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// Close 停止订阅并等待后台协程退出。
func (s *Subscription) Close() {
	s.once.Do(func() { s.cancel() })
	<-s.done
}

// Subscribe 订阅频道。缺少轮询兜底时拒绝（ErrNoFallback）。Redis 不可达也不会失败：订阅在后台自动重连，期间只有轮询兜底在工作。
func (c *Client) Subscribe(ctx context.Context, ch Channel, o SubscribeOptions) (*Subscription, error) {
	if ch.s == "" {
		return nil, ErrBadKey
	}
	if o.Poll == nil || o.PollInterval <= 0 {
		return nil, ErrNoFallback
	}
	if o.Handler == nil {
		return nil, errors.New("redisx: 缺少 Handler")
	}
	c.lifeMu.Lock()
	if c.closed.Load() {
		c.lifeMu.Unlock()
		return nil, ErrClosed
	}
	c.wg.Add(1) // Client.Close 会等待订阅协程退出（c.stop 关闭即终止）
	c.lifeMu.Unlock()
	sctx, cancel := context.WithCancel(ctx)
	s := &Subscription{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer c.wg.Done()
		defer close(s.done)
		defer cancel()
		go func() {
			select {
			case <-c.stop:
				cancel()
			case <-sctx.Done():
			}
		}()
		c.subscribeLoop(sctx, ch, o)
	}()
	return s, nil
}

func (c *Client) safely(what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("Pub/Sub 回调 panic（已捕获）", zap.String("what", what), zap.String("panic", redact.Panic(r)))
		}
	}()
	fn()
}

func (c *Client) subscribeLoop(ctx context.Context, ch Channel, o SubscribeOptions) {
	poll := func() {
		c.safely("poll", func() {
			if err := o.Poll(ctx); err != nil && ctx.Err() == nil {
				c.log.Warn("Pub/Sub 轮询兜底失败", zap.String("channel", ch.s), zap.String("error", c.scrub(err)))
			}
		})
	}
	ps := c.rdb.Subscribe(ctx, ch.s) // 不可达时不阻塞/不失败：go-redis 在后台重连并自动重新订阅
	defer func() { _ = ps.Close() }()
	msgs := ps.Channel(redis.WithChannelHealthCheckInterval(time.Second))
	tick := time.NewTicker(o.PollInterval)
	defer tick.Stop()

	poll()
	rec := c.recoveredCh()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-msgs:
			if !ok {
				return
			}
			c.safely("handler", func() { o.Handler(ctx, m.Payload) })
		case <-tick.C:
			poll()
		case <-rec:
			rec = c.recoveredCh()
			poll() // Redis 刚恢复：断线期间的广播已丢失，立即对账
		}
	}
}
