package db

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/db/internal/token"
)

func mint(text string) Query { return FromBuilder(token.New(), text) }

// 维护账号的分区 DDL（Story 1.15、database/README §5.1）。
//
// `ALTER TABLE … REORGANIZE PARTITION` 的表名、分区名与分区边界都不能用 ? 占位符传参，而 Query 只接受编译期常量，
// 因此这条语句只能由本包这个受信任的构建点生成：表名/分区名经严格白名单校验、边界值只接受 time.Time 并由本包格式化，
// 调用方无法借此拼接任意 SQL。它只接受 maint 池（请求路径不会持有）。

var (
	reMaintTable = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	rePartName   = regexp.MustCompile(`^p[0-9]{6,8}$`)
)

// PartitionSpec 是要新建的一个 RANGE COLUMNS 分区：[上一个分区的上界, LessThan)。
type PartitionSpec struct {
	Name     string    // pYYYYMM 或 pYYYYMMDD
	LessThan time.Time // 分区上界（不含），按 UTC 格式化为 'YYYY-MM-DD HH:MM:SS'
}

// ReorganizePmax 把兜底分区 pmax 拆分为 specs（须按 LessThan 严格递增）加一个新的 pmax：
//
//	ALTER TABLE t REORGANIZE PARTITION pmax INTO (PARTITION p… VALUES LESS THAN ('…'), …, PARTITION pmax VALUES LESS THAN (MAXVALUE))
//
// 不改变任何已有数据（落在新分区边界内的 pmax 行被搬入新分区）；失败时 MySQL 保持原分区不变。
func (p *MaintPool) ReorganizePmax(ctx context.Context, table string, specs []PartitionSpec) error {
	if !reMaintTable.MatchString(table) {
		return fmt.Errorf("db: 非法的分区表名 %q", table)
	}
	if len(specs) == 0 {
		return errors.New("db: REORGANIZE PARTITION 至少需要一个新分区")
	}
	var b strings.Builder
	b.WriteString("ALTER TABLE `" + table + "` REORGANIZE PARTITION pmax INTO (")
	var prev time.Time
	for i, s := range specs {
		if !rePartName.MatchString(s.Name) {
			return fmt.Errorf("db: 非法的分区名 %q", s.Name)
		}
		if s.LessThan.IsZero() || (i > 0 && !s.LessThan.After(prev)) {
			return errors.New("db: 分区上界必须严格递增")
		}
		prev = s.LessThan
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("PARTITION " + s.Name + " VALUES LESS THAN ('" + s.LessThan.UTC().Format("2006-01-02 15:04:05") + "')")
	}
	b.WriteString(", PARTITION pmax VALUES LESS THAN (MAXVALUE))")
	_, err := p.Executor(ctx).Exec(ctx, mint(b.String()))
	if err != nil {
		return fmt.Errorf("db: 分区表 %s REORGANIZE PARTITION: %w", table, err)
	}
	return nil
}
