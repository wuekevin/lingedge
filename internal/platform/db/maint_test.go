package db

import (
	"context"
	"testing"
	"time"
)

// ReorganizePmax 的标识符/边界校验在触达数据库之前完成：非法输入不会生成任何 SQL（空池也不会被使用）。
func TestReorganizePmaxRejectsUnsafeInput(t *testing.T) {
	p := &MaintPool{}
	ok := PartitionSpec{Name: "p202801", LessThan: time.Date(2028, 2, 1, 0, 0, 0, 0, time.UTC)}
	cases := map[string]struct {
		table string
		specs []PartitionSpec
	}{
		"表名含注入":    {"audit_logs`; DROP TABLE users; --", []PartitionSpec{ok}},
		"表名大写":     {"Audit", []PartitionSpec{ok}},
		"空表名":      {"", []PartitionSpec{ok}},
		"无新分区":     {"audit_logs", nil},
		"分区名含注入":   {"audit_logs", []PartitionSpec{{Name: "p1); DROP TABLE x;--", LessThan: ok.LessThan}}},
		"分区名不是 pN": {"audit_logs", []PartitionSpec{{Name: "pmax", LessThan: ok.LessThan}}},
		"上界为零值":    {"audit_logs", []PartitionSpec{{Name: "p202801"}}},
		"上界非递增":    {"audit_logs", []PartitionSpec{ok, {Name: "p202802", LessThan: ok.LessThan}}},
	}
	for name, c := range cases {
		if err := p.ReorganizePmax(context.Background(), c.table, c.specs); err == nil {
			t.Errorf("%s: 应被拒绝", name)
		}
	}
}
