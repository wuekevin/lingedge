package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProtoSymbols(t *testing.T) {
	root := filepath.Join(repoRoot(t), "proto", "lingedge", "node", "v1")
	if fs := checkProtoSymbols(root); len(fs) != 0 {
		t.Fatalf("真实 proto 应含全部符号: %v", fs)
	}
	// 缺符号：复制后删去 held_seq 与 lineage。
	dir := t.TempDir()
	files, _ := filepath.Glob(filepath.Join(root, "*.proto"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		s := strings.ReplaceAll(strings.ReplaceAll(string(b), "held_seq", "x_seq"), "lineage", "x_line")
		_ = os.WriteFile(filepath.Join(dir, filepath.Base(f)), []byte(s), 0o644)
	}
	fs := checkProtoSymbols(dir)
	got := map[string]bool{}
	for _, f := range fs {
		got[f.Where] = true
	}
	if !got["held_seq"] || !got["lineage"] || len(fs) != 2 {
		t.Fatalf("应恰好缺 held_seq 与 lineage: %v", fs)
	}
	// 仅在注释里提到符号不算存在。
	commentOnly := t.TempDir()
	for _, f := range files {
		b, _ := os.ReadFile(f)
		s := strings.ReplaceAll(strings.ReplaceAll(string(b), "held_seq =", "x_seq ="), "rpc GetCaBundle(", "rpc GetCaBundleX(")
		s += "\n// held_seq GetCaBundle( rpc GetCaBundle(\n"
		_ = os.WriteFile(filepath.Join(commentOnly, filepath.Base(f)), []byte(s), 0o644)
	}
	cm := map[string]bool{}
	for _, f := range checkProtoSymbols(commentOnly) {
		cm[f.Where] = true
	}
	if !cm["held_seq"] || !cm["GetCaBundle"] {
		t.Fatalf("注释中提及不应满足符号检查: %v", cm)
	}
	// 少文件。
	_ = os.Remove(filepath.Join(dir, "task.proto"))
	if fs := checkProtoSymbols(dir); !hasRule(fs, "proto") || !strings.Contains(fs[0].Msg, "6 个") {
		t.Fatalf("少于 6 个 .proto 应失败: %v", fs)
	}
}

func TestM1OperationCount(t *testing.T) {
	doc := mustLoad(t, goodSpec())
	if fs := checkM1Operations(doc, 3); len(fs) != 0 {
		t.Fatalf("3 个 M1 operation: %v", fs)
	}
	if fs := checkM1Operations(doc, 200); len(fs) != 1 {
		t.Fatalf("数量不符应失败: %v", fs)
	}
	real := mustLoad(t, []byte(readRepoFile(t, "docs/api/openapi.yaml")))
	if fs := checkM1Operations(real, 200); len(fs) != 0 {
		t.Fatalf("真实契约 M1 应为 200: %v", fs)
	}
}

func TestMigrationOrderMatchesSkeleton(t *testing.T) {
	readme := readRepoFile(t, "docs/database/README.md")
	skel := readRepoFile(t, "docs/database/verification/m1-fk-skeleton.sql")
	if fs := checkMigrationOrder(readme, skel, 61); len(fs) != 0 {
		t.Fatalf("真实文档与骨架应一致: %v", fs)
	}
	rows, _ := parseReadmeMigrations(readme)
	secs, extra := parseSkeleton(skel)
	if len(rows) != 13 || len(secs) != 12 || len(extra) != 6 {
		t.Errorf("解析规模异常: README %d 行、骨架 %d 节、后续阶段样例 %d 张", len(rows), len(secs), len(extra))
	}
}

func TestMigrationOrderNegative(t *testing.T) {
	readme := readRepoFile(t, "docs/database/README.md")
	skel := readRepoFile(t, "docs/database/verification/m1-fk-skeleton.sql")
	cases := map[string]struct{ readme, skel, want string }{
		"骨架把 sites_core 与 certificates 对调": {readme, strings.Replace(strings.Replace(skel, "-- ==== 000007 sites_core", "-- ==== 000008 sites_core", 1), "-- ==== 000008 certificates", "-- ==== 000007 certificates", 1), "递增"},
		"README 命名与骨架不符":                   {strings.Replace(readme, "`sites_core`", "`site_core`", 1), skel, "命名"},
		"README 把 certificates 挪到 000007":  {strings.Replace(readme, "| 000007 | `sites_core` | site_groups、", "| 000007 | `sites_core` | certificates、site_groups、", 1), skel, "certificates"},
		"骨架多出一张 M1 表":                      {readme, strings.Replace(skel, "-- ==== 000013 observability", "CREATE TABLE extra_table (id BIGINT);\n-- ==== 000013 observability", 1), "61"},
		"README 漏写迁移文件":                    {strings.Replace(readme, "| 000013 | `observability` |", "| 000014 | `observability` |", 1), skel, "序号"},
	}
	for name, c := range cases {
		if c.readme == readme && c.skel == skel {
			t.Fatalf("%s: 样例替换未生效", name)
		}
		fs := checkMigrationOrder(c.readme, c.skel, 61)
		if len(fs) == 0 {
			t.Errorf("%s: 应失败", name)
			continue
		}
		hit := false
		for _, f := range fs {
			hit = hit || strings.Contains(f.Msg, c.want)
		}
		if !hit {
			t.Errorf("%s: 违规信息应含 %q: %v", name, c.want, fs)
		}
	}
}

func TestEnvPendingRegistered(t *testing.T) {
	epics := readRepoFile(t, "docs/bmad/epics.md")
	if fs := checkEnvPendingRegistered(epics); len(fs) != 0 {
		t.Fatalf("5 项待环境验证应已登记: %v", fs)
	}
	// 从 Story 3.24 小节抹掉 PoC-5 的登记。
	sec := storySection(epics, "3.24")
	if sec == "" {
		t.Fatal("找不到 Story 3.24")
	}
	if fs := checkEnvPendingRegistered(strings.Replace(epics, sec, strings.ReplaceAll(sec, "PoC-5", "PoC-x"), 1)); len(fs) == 0 {
		t.Error("PoC-5 登记被抹掉应失败")
	}
	if fs := checkEnvPendingRegistered(strings.Replace(epics, "### Story 1.9：", "### Story 1.9x：", 1)); len(fs) == 0 {
		t.Error("Story 缺失应失败")
	}
	if fs := checkEnvPendingRegistered(strings.ReplaceAll(epics, "等待环境验证", "已验证")); len(fs) == 0 {
		t.Error("登记语被抹掉应失败")
	}
}

func TestCF1Record(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "CF-1.md")
	if fs := checkCF1Record(p); len(fs) != 1 {
		t.Errorf("记录不存在应失败: %v", fs)
	}
	_ = os.WriteFile(p, []byte("没有引用\n"), 0o644)
	if fs := checkCF1Record(p); len(fs) != 1 {
		t.Errorf("未引用修订日志应失败: %v", fs)
	}
	_ = os.WriteFile(p, []byte("见 docs/bmad/architecture-revision-log.md\n"), 0o644)
	if fs := checkCF1Record(p); len(fs) != 0 {
		t.Errorf("应通过: %v", fs)
	}
}

func TestTaskTypeScope(t *testing.T) {
	real := mustLoad(t, []byte(readRepoFile(t, "docs/api/openapi.yaml")))
	docs := map[string]string{}
	for _, pat := range []string{"docs/database/*.md", "docs/api/*.md"} {
		ms, _ := filepath.Glob(filepath.Join(repoRoot(t), pat))
		for _, m := range ms {
			b, _ := os.ReadFile(m)
			docs[m] = string(b)
		}
	}
	if fs := checkTaskTypeScope(real, 25, docs); len(fs) != 0 {
		t.Fatalf("真实契约与文档应符合 AA-8 口径: %v", fs)
	}
	// 负向 1：内部周期作业被放进 TaskType。
	polluted := strings.Replace(readRepoFile(t, "docs/api/openapi.yaml"), "      - deployment.retry\n", "      - deployment.retry\n      - partition.maintain\n", 1)
	if fs := checkTaskTypeScope(mustLoad(t, []byte(polluted)), 25, nil); len(fs) < 2 {
		t.Fatalf("TaskType 含内部周期作业应同时违反数量与不相交: %v", fs)
	}
	// 负向 2：文档提到内部周期作业却未标注口径。
	if fs := checkTaskTypeScope(real, 25, map[string]string{"x.md": "Worker 的 `partition.maintain` 任务每日运行\n"}); len(fs) != 1 {
		t.Fatalf("未标注口径应失败: %v", fs)
	}
	if fs := checkTaskTypeScope(real, 25, map[string]string{"x.md": "内部周期作业 `partition.maintain` 每日运行\n"}); len(fs) != 0 {
		t.Fatalf("已标注应通过: %v", fs)
	}
}
