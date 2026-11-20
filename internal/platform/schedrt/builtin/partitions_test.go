package builtin

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func mk(name string, parts ...string) PartitionedTable {
	t := PartitionedTable{Name: name, Method: "RANGE COLUMNS"}
	for i := 0; i+1 < len(parts); i += 2 {
		t.Partitions = append(t.Partitions, Partition{Name: parts[i], Description: parts[i+1]})
	}
	return t
}

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestPlanMonthly(t *testing.T) {
	tb := mk("audit_logs", "p202711", "'2027-12-01 00:00:00'", "p202712", "'2028-01-01 00:00:00'", "pmax", "MAXVALUE")
	add, err := PlanPartitions(tb, day("2027-11-15"), PartitionPolicy{AheadMonths: 3, MaxNewPerRun: 50})
	if err != nil {
		t.Fatal(err)
	}
	// 当前月 2027-11 + 3 个月 = 到 2028-02（含）：已有到 2027-12，需新建 p202801、p202802。
	if len(add) != 2 || add[0].Name != "p202801" || !add[0].LessThan.Equal(day("2028-02-01")) || add[1].Name != "p202802" || !add[1].LessThan.Equal(day("2028-03-01")) {
		t.Fatalf("按月预建: %+v", add)
	}
	// 已足够：不新建。
	if add, err := PlanPartitions(tb, day("2027-09-15"), PartitionPolicy{AheadMonths: 3, MaxNewPerRun: 50}); err != nil || len(add) != 0 {
		t.Fatalf("已足够应为空: %+v %v", add, err)
	}
	// pmax 积压：now 远超最后分区，从最后边界连续补齐（不跳过中间月份），受 MaxNewPerRun 限制。
	add, err = PlanPartitions(tb, day("2028-06-10"), PartitionPolicy{AheadMonths: 1, MaxNewPerRun: 4})
	if err != nil || len(add) != 4 || add[0].Name != "p202801" || add[3].Name != "p202804" {
		t.Fatalf("积压补齐: %+v %v", add, err)
	}
}

func TestPlanDaily(t *testing.T) {
	tb := mk("node_heartbeats", "p20261126", "'2026-11-27 00:00:00'", "p20261127", "'2026-11-28 00:00:00'", "pmax", "MAXVALUE")
	add, err := PlanPartitions(tb, day("2026-11-27"), PartitionPolicy{AheadDays: 3, MaxNewPerRun: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(add) != 3 || add[0].Name != "p20261128" || add[2].Name != "p20261130" || !add[2].LessThan.Equal(day("2026-12-01")) {
		t.Fatalf("按日预建: %+v", add)
	}
}

func TestPlanRejectsUnsupportedShapes(t *testing.T) {
	cases := map[string]PartitionedTable{
		"无 pmax":   mk("t", "p202711", "'2027-12-01 00:00:00'", "p202712", "'2028-01-01 00:00:00'"),
		"仅 pmax":   mk("t", "pmax", "MAXVALUE"),
		"命名与上界不符":  mk("t", "p202701", "'2027-12-01 00:00:00'", "pmax", "MAXVALUE"),
		"月分区上界未对齐": mk("t", "p202711", "'2027-12-02 00:00:00'", "pmax", "MAXVALUE"),
		"未知命名":     mk("t", "pold", "'2027-12-01 00:00:00'", "pmax", "MAXVALUE"),
		"上界无法解析":   mk("t", "p202711", "'garbage'", "pmax", "MAXVALUE"),
		"LIST 分区":  {Name: "t", Method: "LIST", Partitions: []Partition{{Name: "p1", Description: "1"}, {Name: "pmax", Description: "MAXVALUE"}}},
	}
	for name, tb := range cases {
		_, err := PlanPartitions(tb, day("2027-11-15"), DefaultPartitionPolicy)
		if !errors.Is(err, ErrUnsupportedPartitioning) || !strings.Contains(err.Error(), "表 t") {
			t.Errorf("%s: 应拒绝且不改表: %v", name, err)
		}
	}
}
