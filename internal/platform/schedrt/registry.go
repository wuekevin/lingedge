package schedrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/taskrt"
)

// 作业默认超时（内部型作业的窗口租约与执行上限）。[ASSUMPTION] 工程缺省，基线未规定。
const DefaultJobTimeout = 5 * time.Minute

var reJobName = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*){0,3}$`)

// ErrSkipped 标记「本次明确没有执行」（例如缺少所需配置）。作业返回 Skip(...) 时运行器记为 skipped 并告警，不记为成功。
var ErrSkipped = errors.New("schedrt: 作业已跳过")

// Skip 构造一个「明确跳过」的错误：reason 说明为什么没有执行（会进入告警日志）。
func Skip(reason string) error { return fmt.Errorf("%w: %s", ErrSkipped, reason) }

// JobFunc 是内部型作业的执行体。fire 是触发窗口的起点（UTC）。必须幂等：窗口租约失效（超时）或停顿重叠时可能被再次执行。
type JobFunc func(ctx context.Context, fire time.Time) error

// TaskSpec 描述任务型作业入队的任务。
type TaskSpec struct {
	// Type 必须是 OpenAPI TaskType（taskrt.IsKnown），且在注册表中登记了处理函数（否则入队被拒，作业记为失败并告警）。
	Type taskrt.Type
	// Payload 生成任务 payload（JSON 对象，经 taskrt.ValidatePayload）；nil = {"scheduled_for": <窗口起点 RFC 3339>}。
	Payload func(fire time.Time) (json.RawMessage, error)
	// Priority/MaxAttempts 零值取注册表里该类型的默认。
	Priority    int
	MaxAttempts uint32
}

// Job 是一个周期作业。Run 与 Task 恰好设置其一。
type Job struct {
	Name     string
	Schedule Schedule
	// Timeout 是内部型作业的执行上限（缺省 DefaultJobTimeout）；也是窗口租约在执行期间的有效期。
	Timeout time.Duration
	// Run：内部型（不属 TaskType，AA-8）。
	Run JobFunc
	// Task：任务型（以确定性幂等键入队 OpenAPI TaskType）。
	Task *TaskSpec
}

// Kind 返回作业类型的中文名（日志用）。
func (j Job) Kind() string {
	if j.Task != nil {
		return "task"
	}
	return "internal"
}

// IdempotencyKey 是任务型作业在窗口 fire 的确定性幂等键（≤128 字符）。
func IdempotencyKey(name string, fire time.Time) string {
	return fmt.Sprintf("cron:%s:%d", name, fire.UTC().Unix())
}

// Registry 是 cron 注册表：内部作业与后续 Story 的周期任务都经它登记。启动（Runner.Run）前登记完毕。
type Registry struct {
	mu   sync.Mutex
	jobs map[string]Job
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry { return &Registry{jobs: map[string]Job{}} }

// Internal 登记内部型作业（直接在持有者进程内执行；名称不得与任何 OpenAPI TaskType 同名——它们不属于 TaskType，AA-8）。
func (r *Registry) Internal(name string, s Schedule, timeout time.Duration, fn JobFunc) error {
	return r.add(Job{Name: name, Schedule: s, Timeout: timeout, Run: fn})
}

// Task 登记任务型作业（周期性地以确定性幂等键入队 OpenAPI TaskType，如 heartbeat.sweep、rollup.aggregate）。
func (r *Registry) Task(name string, s Schedule, spec TaskSpec) error {
	return r.add(Job{Name: name, Schedule: s, Task: &spec})
}

func (r *Registry) add(j Job) error {
	switch {
	case !reJobName.MatchString(j.Name) || len(j.Name) > 64:
		return fmt.Errorf("schedrt: 作业名 %q 非法（小写字母/数字/下划线，以点分段，最长 64）", j.Name)
	case j.Schedule == nil:
		return fmt.Errorf("schedrt: 作业 %s 缺少调度规则", j.Name)
	case (j.Run == nil) == (j.Task == nil):
		return fmt.Errorf("schedrt: 作业 %s 必须且只能设置 Run（内部型）或 Task（任务型）之一", j.Name)
	case j.Timeout < 0:
		return fmt.Errorf("schedrt: 作业 %s 的超时不得为负", j.Name)
	}
	if j.Task != nil {
		if !taskrt.IsKnown(j.Task.Type) {
			return fmt.Errorf("schedrt: 作业 %s 的任务类型 %q 不在 OpenAPI TaskType 白名单内", j.Name, j.Task.Type)
		}
	} else if taskrt.IsKnown(taskrt.Type(j.Name)) {
		return fmt.Errorf("schedrt: 内部作业名 %q 与 OpenAPI TaskType 同名：内部周期作业不属 TaskType（AA-8）；需要走任务队列请用 Registry.Task", j.Name)
	}
	if j.Timeout == 0 {
		j.Timeout = DefaultJobTimeout
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.jobs[j.Name]; dup {
		return fmt.Errorf("schedrt: 作业 %s 重复登记", j.Name)
	}
	r.jobs[j.Name] = j
	return nil
}

// Jobs 按名称升序返回全部作业。
func (r *Registry) Jobs() []Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Name < out[k].Name })
	return out
}

// Lookup 按名称取作业。
func (r *Registry) Lookup(name string) (Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[name]
	return j, ok
}
