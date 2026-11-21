package installer

import (
	"sync/atomic"
	"time"
)

// EventType 是进度事件的类型。
type EventType string

// EventType 取值。
const (
	EventPhase       EventType = "phase"        // 进入某阶段
	EventStepStart   EventType = "step_start"   // 某步开始（journal 的 begin 已落盘）
	EventStepDone    EventType = "step_done"    // 某步完成
	EventStepSkipped EventType = "step_skipped" // 某步无需执行（--skip-start、keep 的组件、续跑时已完成）
	EventStepFailed  EventType = "step_failed"
	EventWarning     EventType = "warning"
	EventRollback    EventType = "rollback_step" // 回滚中某步被撤销
	EventInfo        EventType = "info"
)

// Event 是 installer 发给 UI（CLI 进度 NDJSON / 将来的 TUI）的进度事件。Detail 只含路径/计数等非敏感信息——
// setup token、master.key 内容、口令绝不进入事件（测试以明文扫描断言）。
type Event struct {
	Type      EventType      `json:"type"`
	Step      int            `json:"step,omitempty"`
	Name      string         `json:"name,omitempty"`
	Phase     Phase          `json:"phase,omitempty"`
	Component string         `json:"component,omitempty"`
	Code      string         `json:"code,omitempty"`
	Message   string         `json:"message,omitempty"`
	Time      string         `json:"ts"`
	Detail    map[string]any `json:"detail,omitempty"`
}

// emitter 把事件非阻塞地投递到 Deps.Events：通道满（慢消费者/无人读取）就丢弃并计数，通道被关闭导致的 panic 被吞掉。
// UI 因此永远不可能拖住、拖垮或改变安装结果（AC1）。
type emitter struct {
	ch      chan<- Event
	now     func() time.Time
	dropped atomic.Int64
}

func (e *emitter) emit(ev Event) {
	if e == nil || e.ch == nil {
		return
	}
	if ev.Time == "" {
		ev.Time = e.now().UTC().Format("2006-01-02T15:04:05.000Z")
	}
	defer func() {
		if recover() != nil { // 向已关闭的通道发送会 panic
			e.dropped.Add(1)
		}
	}()
	select {
	case e.ch <- ev:
	default:
		e.dropped.Add(1)
	}
}

// Dropped 返回被丢弃的事件数。
func (e *emitter) Dropped() int64 {
	if e == nil {
		return 0
	}
	return e.dropped.Load()
}
