package service

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Ensurer.Plan（Story 1.33）是 Ensure 的只读预演：它预告的「将创建的目录」必须与 Ensure 实际创建的逐项一致（含缺失祖先与同批次内的父子关系），
// Plan 本身不改动任何东西；既有目录的不符与 Ensure 报告的 MismatchError 一致。
func TestPlanPredictsEnsureExactly(t *testing.T) {
	root := t.TempDir()
	own := &recOwn{Same: true}
	e := &Ensurer{Root: root, Own: own}
	// 预置：/etc 存在（已有 0755），/var/lib 不存在 → 祖先会被创建。
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	specs := append(ServerDirs("lingedge", "lingedge"), NodeDirs("lingedge-node", "lingedge-node")...)
	before := listTree(t, root)
	plans, err := e.Plan(specs)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, listTree(t, root)) {
		t.Fatal("Plan 不得改动文件系统")
	}
	var predicted []string
	for _, p := range plans {
		predicted = append(predicted, p.Create...)
	}
	res, err := e.Ensure(specs)
	if err != nil {
		t.Fatal(err)
	}
	var actual []string
	for _, r := range res {
		if r.Created {
			actual = append(actual, r.Path)
		}
	}
	if !reflect.DeepEqual(predicted, actual) || len(actual) == 0 {
		t.Fatalf("预演与实际不一致\n预演: %v\n实际: %v", predicted, actual)
	}
	// 撤销预判：深到浅，只含将创建者。
	ups := PlanUndo(plans)
	if len(ups) != len(predicted) || ups[0].Path != predicted[len(predicted)-1] || !ups[0].Created {
		t.Fatalf("PlanUndo: %+v", ups)
	}
	// 再次 Plan：全部已存在，无需创建。
	plans, err = e.Plan(specs)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plans {
		if len(p.Create) != 0 || !p.Exists {
			t.Fatalf("%+v", p)
		}
	}
	// 权限宽于规格 → Mismatch，与 Ensure 一致。
	if err := os.Chmod(filepath.Join(root, "etc/lingedge/server"), 0o755); err != nil {
		t.Fatal(err)
	}
	plans, _ = e.Plan(ServerDirs("lingedge", "lingedge"))
	found := false
	for _, p := range plans {
		if p.Path == "/etc/lingedge/server" && len(p.Mismatch) == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("应报告权限不符: %+v", plans)
	}
	if _, err := e.Ensure(ServerDirs("lingedge", "lingedge")); err == nil {
		t.Fatal("Ensure 对同一不符应返回 MismatchError")
	}
}

func listTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel+" "+fi.Mode().String())
		}
		return nil
	})
	return out
}

func TestIsManagedPath(t *testing.T) {
	for p, want := range map[string]bool{"/etc/lingedge/server": true, "/var/lib/lingedge-node/cache": true, "/var/log/lingedge": true, "/var/lib/lingedge/tools": false,
		"/var/log": false, "/etc": false, "/opt/lingedge": false, "/etc/lingedge/../passwd": false} {
		if got := IsManagedPath(p); got != want {
			t.Errorf("IsManagedPath(%q)=%v", p, got)
		}
	}
}
