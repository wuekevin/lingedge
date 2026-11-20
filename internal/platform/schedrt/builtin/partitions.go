package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/schedrt"
	"github.com/wuekevin/lingedge/internal/platform/scope"
)

// PartitionPolicy 是滚动预建的范围。
type PartitionPolicy struct {
	// AheadMonths：按月分区表（pYYYYMM）预建到「当前月 + N 个月」（含）。
	AheadMonths int
	// AheadDays：按日分区表（pYYYYMMDD）预建到「今天 + N 天」（含）；按月预建对日分区表会产生上百个分区，故日分区用天数。
	AheadDays int
	// MaxNewPerRun：单表单次最多新建的分区数（pmax 积压很久时分多次追上，避免一条超大的 REORGANIZE）。
	MaxNewPerRun int
}

// DefaultPartitionPolicy：[ASSUMPTION] 基线未规定 N，取 3 个月 / 14 天——足以覆盖维护作业连续失败数周而写入仍落在正确分区。
var DefaultPartitionPolicy = PartitionPolicy{AheadMonths: 3, AheadDays: 14, MaxNewPerRun: 120}

// Partition 是一个已有分区（来自 information_schema.PARTITIONS）。
type Partition struct {
	Name        string
	Description string // RANGE 上界：'YYYY-MM-DD HH:MM:SS'（带引号）或 MAXVALUE
}

// PartitionedTable 是一张实际存在的分区表及其全部分区（按序）。
type PartitionedTable struct {
	Name       string
	Method     string // RANGE / RANGE COLUMNS
	Partitions []Partition
}

// PartitionStore 是分区维护的持久层契约（实现：schedrt/repository.Partitions，maint 连接池）。
type PartitionStore interface {
	// ListPartitioned 列出 maint 账号可见的、实际存在的分区表（information_schema，当前库）。
	ListPartitioned(ctx context.Context, sc scope.Scope) ([]PartitionedTable, error)
	// ReorganizePmax 把 pmax 拆成 add 里的新分区加新的 pmax。
	ReorganizePmax(ctx context.Context, sc scope.Scope, table string, add []db.PartitionSpec) error
}

// ErrUnsupportedPartitioning：分区表不符合约定的形态（无 pmax、命名/边界无法识别），不会被改动。
var ErrUnsupportedPartitioning = errors.New("分区表形态不受支持")

func parseBound(desc string) (time.Time, error) {
	d := strings.Trim(desc, "'")
	for _, layout := range []string{"2006-01-02 15:04:05.000", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, d, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析分区上界 %q", desc)
}

// PlanPartitions 计算要从 pmax 里拆出的新分区（纯函数，无 I/O）。无需新建时返回空切片。
//
// 约定（database/README §5.1）：RANGE (COLUMNS) 分区，末尾是 pmax VALUES LESS THAN (MAXVALUE)；
// 其余分区命名 pYYYYMM（上界为次月 1 日 00:00:00，按月）或 pYYYYMMDD（上界为次日 00:00:00，按日），名称对应上界前一个周期。
// 不满足约定（无 pmax、命名与上界不一致、无法识别粒度）时返回 ErrUnsupportedPartitioning，绝不猜测着改表。
func PlanPartitions(t PartitionedTable, now time.Time, p PartitionPolicy) ([]db.PartitionSpec, error) {
	unsupported := func(format string, a ...any) error {
		return fmt.Errorf("%w: 表 %s "+format, append([]any{ErrUnsupportedPartitioning, t.Name}, a...)...)
	}
	if !strings.HasPrefix(t.Method, "RANGE") {
		return nil, unsupported("的分区方式是 %q，只支持 RANGE/RANGE COLUMNS", t.Method)
	}
	n := len(t.Partitions)
	if n < 2 {
		return nil, unsupported("没有可参照的已有分区边界")
	}
	if last := t.Partitions[n-1]; last.Name != "pmax" || !strings.EqualFold(last.Description, "MAXVALUE") {
		return nil, unsupported("缺少末尾的 pmax（VALUES LESS THAN (MAXVALUE)）兜底分区")
	}
	ref := t.Partitions[n-2]
	bound, err := parseBound(ref.Description)
	if err != nil {
		return nil, unsupported("的分区 %s: %v", ref.Name, err)
	}
	var monthly bool
	switch len(ref.Name) {
	case 7:
		monthly = true
	case 9:
	default:
		return nil, unsupported("的分区名 %s 不是 pYYYYMM/pYYYYMMDD", ref.Name)
	}
	step := func(b time.Time) time.Time {
		if monthly {
			return b.AddDate(0, 1, 0)
		}
		return b.AddDate(0, 0, 1)
	}
	name := func(periodStart time.Time) string {
		if monthly {
			return "p" + periodStart.Format("200601")
		}
		return "p" + periodStart.Format("20060102")
	}
	aligned := bound.Equal(time.Date(bound.Year(), bound.Month(), 1, 0, 0, 0, 0, time.UTC))
	if !monthly {
		aligned = bound.Equal(time.Date(bound.Year(), bound.Month(), bound.Day(), 0, 0, 0, 0, time.UTC))
	}
	prev := bound.AddDate(0, -1, 0)
	if !monthly {
		prev = bound.AddDate(0, 0, -1)
	}
	if !aligned || name(prev) != ref.Name {
		return nil, unsupported("的分区 %s 与其上界 %s 不一致（应为上界前一个周期的名称）", ref.Name, ref.Description)
	}

	now = now.UTC()
	var horizon time.Time // 不含：新分区的起点必须早于它
	if monthly {
		horizon = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, p.AheadMonths+1, 0)
	} else {
		horizon = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, p.AheadDays+1)
	}
	var add []db.PartitionSpec
	for start := bound; start.Before(horizon) && len(add) < p.MaxNewPerRun; start = step(start) {
		add = append(add, db.PartitionSpec{Name: name(start), LessThan: step(start)})
	}
	return add, nil
}

// PartitionMaintain 是 partition.maintain：对 maint 账号可见的、实际存在的分区表逐表滚动预建分区。
//
// 失败语义（database/README §5.1「失败告警但不影响写入」）：任一表失败不阻止其余表；只要有失败就返回错误（运行器记为失败并告警，窗口内短退避重试）。
// pmax 始终保留，因此作业失败或迟到都不会让写入报 ERROR 1526。maint 账号未配置（Partitions 为 nil）时返回 Skip，运行器记为「跳过」并告警，不是成功。
func PartitionMaintain(d Deps) schedrt.JobFunc {
	return func(ctx context.Context, _ time.Time) error {
		if d.Partitions == nil {
			return schedrt.Skip("未配置 lingedge_maint 账号（database.maint.user/password）：分区不会滚动预建，新数据最终落入 pmax 兜底分区（写入不受影响）；请配置维护账号")
		}
		tables, err := d.Partitions.ListPartitioned(ctx, d.Scope)
		if err != nil {
			return fmt.Errorf("列出分区表: %w", err)
		}
		if len(tables) == 0 {
			d.Logger.Warn("maint 账号看不到任何分区表（未授权或尚无分区表），本次没有需要维护的对象")
			return nil
		}
		now := d.Clock.Now()
		var errs []error
		for _, t := range tables {
			add, err := PlanPartitions(t, now, d.Policy)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if len(add) == 0 {
				d.Logger.Debug("分区已足够，无需预建", zap.String("table", t.Name))
				continue
			}
			if err := d.Partitions.ReorganizePmax(ctx, d.Scope, t.Name, add); err != nil {
				errs = append(errs, fmt.Errorf("表 %s: %w", t.Name, err))
				continue
			}
			d.Logger.Info("已滚动预建分区", zap.String("table", t.Name), zap.Int("added", len(add)), zap.String("first", add[0].Name), zap.String("last", add[len(add)-1].Name))
		}
		return errors.Join(errs...)
	}
}
