package migrate

import (
	"bytes"
	"encoding/json"
	"testing"
	"testing/fstest"
)

func mustLoad(t *testing.T, fsys fstest.MapFS) *Set {
	t.Helper()
	set, findings, err := Load(fsys)
	if err != nil || len(findings) != 0 {
		t.Fatalf("Load: err=%v findings=%v", err, findings)
	}
	return set
}

func samplePlanFS() fstest.MapFS {
	return fstest.MapFS{
		"000001_core.sql":      {Data: []byte(goodExpand)},
		"000002_add_col.sql":   {Data: []byte(hdr("reversible") + "ALTER TABLE t ADD COLUMN c INT NULL;\n-- +goose Down\nALTER TABLE t DROP COLUMN c;\n")},
		"000003_drop_col.sql":  {Data: []byte(hdr("irreversible") + "ALTER TABLE t DROP COLUMN old;\n-- +goose Down\n-- lingedge:reason=数据已删除\nSELECT 'irreversible';\n")},
		"000004_more_core.sql": {Data: []byte("-- lingedge:class=expand\n-- lingedge:summary=第二张表\n-- +goose Up\nCREATE TABLE IF NOT EXISTS u (id INT) ENGINE=InnoDB;\nGRANT SELECT, INSERT, UPDATE, DELETE ON u TO lingedge_app;\n")},
	}
}

func TestBuildPlanListsAllWhenOffline(t *testing.T) {
	p := BuildPlan(mustLoad(t, samplePlanFS()), nil)
	if p.Mode != "all" || p.Count != 4 || p.LatestKnown != 4 || !p.HasIrreversible || p.MaxClass != ClassIrreversible {
		t.Fatalf("离线计划错误: %+v", p)
	}
	byV := map[int64]PlanItem{}
	for _, it := range p.Migrations {
		byV[it.Version] = it
	}
	if byV[1].Class != ClassExpand || byV[1].HasDown || byV[1].Name != "core" || byV[1].File != "000001_core.sql" {
		t.Errorf("000001: %+v", byV[1])
	}
	if byV[2].Class != ClassReversible || !byV[2].HasDown {
		t.Errorf("000002: %+v", byV[2])
	}
	if byV[3].Class != ClassIrreversible || byV[3].HasDown || byV[3].Reason == "" {
		t.Errorf("000003 的 irreversible 标记不算可执行 Down，且应带原因: %+v", byV[3])
	}
	if byV[4].Summary != "第二张表" || len(byV[4].Checksum) != 64 {
		t.Errorf("000004: %+v", byV[4])
	}
}

func TestBuildPlanPendingOnlyAndHasIrreversible(t *testing.T) {
	set := mustLoad(t, samplePlanFS())
	// 已应用 1、2、3：待执行只剩 4（expand）→ 不含不可逆。
	p := BuildPlan(set, map[int64]bool{1: true, 2: true, 3: true})
	if p.Mode != "pending" || p.Count != 1 || p.HasIrreversible || p.MaxClass != ClassExpand {
		t.Fatalf("待执行计划错误: %+v", p)
	}
	// 已应用 1：待执行含 irreversible。
	p = BuildPlan(set, map[int64]bool{1: true})
	if p.Count != 3 || !p.HasIrreversible {
		t.Fatalf("待执行含不可逆时 has_irreversible 必须为 true: %+v", p)
	}
	// 全部已应用。
	p = BuildPlan(set, map[int64]bool{1: true, 2: true, 3: true, 4: true})
	if p.Count != 0 || p.HasIrreversible || p.MaxClass != "" || p.Migrations == nil {
		t.Fatalf("无待执行时 migrations 必须是空数组而不是 null: %+v", p)
	}
}

func TestPlanJSONShape(t *testing.T) {
	var buf bytes.Buffer
	if err := BuildPlan(mustLoad(t, samplePlanFS()), nil).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("plan 必须是合法 JSON: %v\n%s", err, buf.String())
	}
	for _, k := range []string{"mode", "latest_version", "count", "migrations", "has_irreversible"} {
		if _, ok := got[k]; !ok {
			t.Errorf("plan JSON 缺少键 %q", k)
		}
	}
	first := got["migrations"].([]any)[0].(map[string]any)
	for _, k := range []string{"version", "name", "class", "has_down", "file", "checksum"} {
		if _, ok := first[k]; !ok {
			t.Errorf("plan 项缺少键 %q（AC1：序号、名称、分类、是否含 Down）", k)
		}
	}
}
