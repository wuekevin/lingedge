package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/registry"
)

func mf(t *testing.T, vers ...manifest.VersionEntry) *manifest.Manifest {
	t.Helper()
	m := &manifest.Manifest{Components: map[string]manifest.Component{"server": {Status: manifest.CompAvailable, Versions: vers}, "dns": {Status: manifest.CompUnreleased}, "scheduler": {Status: manifest.CompBuiltin, BuiltinIn: "server"}}}
	for i := range m.Components["server"].Versions {
		v, err := manifest.ParseVersion(m.Components["server"].Versions[i].Version)
		if err != nil {
			t.Fatal(err)
		}
		m.Components["server"].Versions[i].Parsed = v
	}
	return m
}

func state(comps map[string]string) registry.State {
	st := registry.NewState()
	for n, v := range comps {
		st.Components[n] = registry.Component{Version: v, State: "running"}
	}
	return st
}

func TestCheckStatusesAndMigrationNotice(t *testing.T) {
	m := mf(t,
		manifest.VersionEntry{Version: "1.0.0"},
		manifest.VersionEntry{Version: "1.1.0", Migrations: &manifest.MigrationsNotice{Classes: []string{"expand"}, IrreversibleCount: 0, BackupRequired: true, SchemaFrom: "000018", SchemaTo: "000019"}},
		manifest.VersionEntry{Version: "1.2.0", NotesURL: "https://x/notes", Migrations: &manifest.MigrationsNotice{Classes: []string{"reversible", "irreversible"}, IrreversibleCount: 1, BackupRequired: true}},
		manifest.VersionEntry{Version: "1.3.0-rc.1"},
	)
	es := Check(m, state(map[string]string{"server": "1.0.0"}))
	if len(es) != 1 {
		t.Fatal(es)
	}
	e := es[0]
	if e.Status != StatusUpgradeAvailable || e.Latest != "1.2.0" || e.Migrations == nil || !e.Migrations.HasIrreversible || e.Migrations.IrreversibleCount != 1 ||
		!reflect.DeepEqual(e.Migrations.Classes, []string{"reversible", "irreversible"}) || e.NotesURL != "https://x/notes" {
		t.Fatalf("%+v", e)
	}
	// 已是最新（预发布不算稳定版）。
	if e := Check(m, state(map[string]string{"server": "1.2.0"}))[0]; e.Status != StatusUpToDate || e.Migrations != nil {
		t.Fatalf("%+v", e)
	}
	// 未发布/内置/清单没有。
	es = Check(m, state(map[string]string{"dns": "0.1.0", "scheduler": "1.0.0", "ghost": "1.0.0"}))
	got := map[string]string{}
	for _, x := range es {
		got[x.Component] = x.Status
	}
	if got["dns"] != StatusUnreleased || got["scheduler"] != StatusBuiltin || got["ghost"] != StatusNotInManifest {
		t.Fatalf("%v", got)
	}
}

func TestCheckBlockedStepwiseAndRevoked(t *testing.T) {
	m := mf(t,
		manifest.VersionEntry{Version: "1.0.0"},
		manifest.VersionEntry{Version: "1.5.0", MinUpgradeFrom: "1.0.0", Migrations: &manifest.MigrationsNotice{Classes: []string{"expand"}}},
		manifest.VersionEntry{Version: "2.0.0", MinUpgradeFrom: "1.5.0"},
	)
	e := Check(m, state(map[string]string{"server": "1.0.0"}))[0]
	if e.Status != StatusBlocked || e.MinUpgradeFrom != "1.5.0" || !reflect.DeepEqual(e.UpgradePath, []string{"1.5.0", "2.0.0"}) {
		t.Fatalf("%+v", e)
	}
	if e.Migrations == nil || !reflect.DeepEqual(e.Migrations.Classes, []string{"expand"}) {
		t.Fatalf("blocked 时迁移预告应是「下一步」(1.5.0) 的: %+v", e.Migrations)
	}
	// 已装版本被吊销：提示尽快升级；被吊销的版本不作为升级目标（经真实解析的清单，吊销表就绪）。
	doc := `{"schema_version":1,"channel":"stable","sequence":3,"generated_at":"2026-11-01T00:00:00Z","valid_until":"2027-02-01T00:00:00Z",
"revoked_versions":[{"component":"server","version":"1.0.0","reason":"CVE"},{"component":"server","version":"1.2.0","reason":"bad build"}],
"components":{"server":{"status":"available","versions":[
 {"version":"1.0.0","variants":{"default":{"artifacts":{"any":{"url":"https://r.example/a.tgz","sha256":"` + strings.Repeat("a", 64) + `","size":10}}}}},
 {"version":"1.1.0","variants":{"default":{"artifacts":{"any":{"url":"https://r.example/b.tgz","sha256":"` + strings.Repeat("b", 64) + `","size":10}}}}},
 {"version":"1.2.0","variants":{"default":{"artifacts":{"any":{"url":"https://r.example/c.tgz","sha256":"` + strings.Repeat("c", 64) + `","size":10}}}}}]}}}`
	rm, err := manifest.ParseManifest([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	r := Check(rm, state(map[string]string{"server": "1.0.0"}))[0]
	if !r.InstalledRevoked || r.Latest != "1.1.0" || r.Status != StatusUpgradeAvailable || !strings.Contains(r.Message, "吊销") {
		t.Fatalf("%+v", r)
	}
}

func fakeExe(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSwapBinary(t *testing.T) {
	dir := t.TempDir()
	target := fakeExe(t, dir, "lingedge", "OLD")
	src := fakeExe(t, t.TempDir(), "src", "NEW")
	// 自检失败：target 原封不动、.new 被删除、没有 .prev。
	_, err := SwapBinary(context.Background(), target, src, func(context.Context, string) error { return errors.New("boom") })
	if err == nil || !strings.Contains(err.Error(), "自检失败") {
		t.Fatalf("%v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "OLD" {
		t.Fatal("自检失败后 target 必须不变")
	}
	for _, n := range []string{"lingedge.new", "lingedge.prev"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			t.Errorf("%s 不应存在", n)
		}
	}
	// 成功：自检看到的是 .new（0755）；之后 target 是新内容，.prev 是旧内容；两者都可执行。
	var checked string
	prev, err := SwapBinary(context.Background(), target, src, func(_ context.Context, p string) error {
		checked = p
		b, _ := os.ReadFile(p)
		if string(b) != "NEW" {
			return errors.New("自检看到的不是新内容")
		}
		return nil
	})
	if err != nil || prev != target+".prev" || checked != target+".new" {
		t.Fatalf("%q %q %v", prev, checked, err)
	}
	if b, _ := os.ReadFile(target); string(b) != "NEW" {
		t.Fatal("target 应是新内容")
	}
	if b, _ := os.ReadFile(prev); string(b) != "OLD" {
		t.Fatal(".prev 应是旧内容")
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o755 {
		t.Errorf("权限 %v", fi.Mode())
	}
	if _, err := os.Stat(target + ".new"); err == nil {
		t.Error(".new 应已被 rename 走")
	}
	// 再升级一次：旧的 .prev 被替换为刚才的版本。
	src2 := fakeExe(t, t.TempDir(), "src2", "NEWER")
	if _, err := SwapBinary(context.Background(), target, src2, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(prev); string(b) != "NEW" {
		t.Fatal(".prev 应是上一版本")
	}
	// target 是符号链接/目录/不存在：拒绝。
	ln := filepath.Join(dir, "ln")
	if err := os.Symlink(target, ln); err != nil {
		t.Fatal(err)
	}
	if _, err := SwapBinary(context.Background(), ln, src, nil); err == nil {
		t.Error("符号链接目标必须拒绝")
	}
	if _, err := SwapBinary(context.Background(), filepath.Join(dir, "nope"), src, nil); err == nil {
		t.Error("不存在必须拒绝")
	}
}
