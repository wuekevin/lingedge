package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const mig1 = "-- lingedge:class=expand\nCREATE TABLE a (id BIGINT);\n"
const mig2 = "-- lingedge:class=expand\nCREATE TABLE b (id BIGINT);\n"

// sealedDir 建一个含两份迁移且都已登记的目录。
func sealedDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "000001_a.sql", mig1)
	writeFile(t, dir, "000002_b.sql", mig2)
	writeFile(t, dir, "CHECKSUMS", "# header\n")
	added, err := sealMigrations(dir)
	if err != nil || len(added) != 2 {
		t.Fatalf("seal: %v %v", added, err)
	}
	return dir
}

func TestMigrationsPublishedUnchangedPasses(t *testing.T) {
	dir := sealedDir(t)
	fs, err := checkMigrations(dir, nil)
	if err != nil || len(fs) != 0 {
		t.Fatalf("未改动应通过: %v %v", fs, err)
	}
}

func TestMigrationsModifiedPublishedFails(t *testing.T) {
	dir := sealedDir(t)
	writeFile(t, dir, "000001_a.sql", mig1+"ALTER TABLE a ADD c INT;\n")
	fs, _ := checkMigrations(dir, nil)
	if len(fs) != 1 || !strings.Contains(fs[0].Msg, "被改动") {
		t.Fatalf("改动已发布迁移必须失败: %v", fs)
	}
	// 改一个字节（空白）也必须失败。
	dir = sealedDir(t)
	writeFile(t, dir, "000002_b.sql", mig2+" ")
	if fs, _ := checkMigrations(dir, nil); len(fs) != 1 {
		t.Fatalf("仅追加空白也必须失败: %v", fs)
	}
}

func TestMigrationsDeletedPublishedFails(t *testing.T) {
	dir := sealedDir(t)
	if err := os.Remove(filepath.Join(dir, "000001_a.sql")); err != nil {
		t.Fatal(err)
	}
	fs, _ := checkMigrations(dir, nil)
	if len(fs) != 1 || !strings.Contains(fs[0].Msg, "缺失") {
		t.Fatalf("删除已发布迁移必须失败: %v", fs)
	}
}

func TestMigrationsNewNeedsClassAnnotation(t *testing.T) {
	dir := sealedDir(t)
	writeFile(t, dir, "000003_c.sql", "CREATE TABLE c (id BIGINT);\n")
	fs, _ := checkMigrations(dir, nil)
	if len(fs) != 1 || !strings.Contains(fs[0].Msg, "lingedge:class") {
		t.Fatalf("新增迁移无标注必须失败: %v", fs)
	}
	writeFile(t, dir, "000003_c.sql", "-- lingedge:class=expand\nCREATE TABLE c (id BIGINT);\n")
	if fs, _ := checkMigrations(dir, nil); len(fs) != 0 {
		t.Fatalf("带标注的新增迁移应通过: %v", fs)
	}
	// 无标注的迁移不能被登记为已发布。
	writeFile(t, dir, "000004_d.sql", "CREATE TABLE d (id BIGINT);\n")
	if _, err := sealMigrations(dir); err == nil {
		t.Fatal("无标注迁移不得被 seal")
	}
}

func TestMigrationsSealAppendOnlyAndIdempotent(t *testing.T) {
	dir := sealedDir(t)
	before, _ := os.ReadFile(filepath.Join(dir, "CHECKSUMS"))
	writeFile(t, dir, "000003_c.sql", "-- lingedge:class=expand\nCREATE TABLE c (id BIGINT);\n")
	added, err := sealMigrations(dir)
	if err != nil || len(added) != 1 || added[0] != "000003_c.sql" {
		t.Fatalf("应只登记新增的一份: %v %v", added, err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "CHECKSUMS"))
	if !strings.HasPrefix(string(after), string(before)) {
		t.Fatal("seal 改写了已有清单行（必须只追加）")
	}
	if again, _ := sealMigrations(dir); len(again) != 0 {
		t.Fatalf("重复 seal 应无新增: %v", again)
	}
}

func TestMigrationsManifestAppendOnlyAgainstBase(t *testing.T) {
	dir := sealedDir(t)
	base, _ := os.ReadFile(filepath.Join(dir, "CHECKSUMS"))
	if fs, _ := checkMigrations(dir, base); len(fs) != 0 {
		t.Fatalf("清单与基线一致应通过: %v", fs)
	}
	// 同时改文件与清单（自洽地篡改）：文件哈希检查看不出，基线比对必须抓住。
	newBody := mig1 + "ALTER TABLE a ADD c INT;\n"
	writeFile(t, dir, "000001_a.sql", newBody)
	h, _ := sha256File(filepath.Join(dir, "000001_a.sql"))
	forged := strings.Replace(string(base), strings.Fields(strings.Split(string(base), "\n")[1])[0], h, 1)
	writeFile(t, dir, "CHECKSUMS", forged)
	fs, _ := checkMigrations(dir, base)
	if len(fs) != 1 || !strings.Contains(fs[0].Msg, "改写") {
		t.Fatalf("改写清单中的已发布哈希必须失败: %v", fs)
	}
	// 连文件带清单行一起删除。
	dir = sealedDir(t)
	base, _ = os.ReadFile(filepath.Join(dir, "CHECKSUMS"))
	_ = os.Remove(filepath.Join(dir, "000002_b.sql"))
	lines := strings.Split(strings.TrimSpace(string(base)), "\n")
	writeFile(t, dir, "CHECKSUMS", strings.Join(lines[:len(lines)-1], "\n")+"\n")
	fs, _ = checkMigrations(dir, base)
	if len(fs) != 1 || !strings.Contains(fs[0].Msg, "删除") {
		t.Fatalf("删除清单条目必须失败: %v", fs)
	}
}

func TestMigrationsMalformedManifest(t *testing.T) {
	for name, body := range map[string]string{
		"哈希过短":   "abc  000001_a.sql\n",
		"单空格分隔":  strings.Repeat("a", 64) + " 000001_a.sql\n",
		"重复登记":   strings.Repeat("a", 64) + "  x.sql\n" + strings.Repeat("b", 64) + "  x.sql\n",
		"大写十六进制": strings.Repeat("A", 64) + "  x.sql\n",
	} {
		dir := t.TempDir()
		writeFile(t, dir, "CHECKSUMS", body)
		fs, err := checkMigrations(dir, nil)
		if err != nil || len(fs) == 0 {
			t.Errorf("%s: 应报告格式错误: %v %v", name, fs, err)
		}
	}
}

// 仓库当前状态：清单可解析，且 migrations/ 下除占位文件外没有未登记的迁移。
func TestRealMigrationsDir(t *testing.T) {
	fs, err := checkMigrations(filepath.Join(repoRoot(t), "migrations"), nil)
	if err != nil || len(fs) != 0 {
		t.Fatalf("真实 migrations/ 应通过: %v %v", fs, err)
	}
}

func TestMigrationsSubdirAndExtensionCase(t *testing.T) {
	dir := sealedDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "sub/000003_c.sql", "CREATE TABLE c (id BIGINT);\n")
	writeFile(t, dir, "000004_d.SQL", "CREATE TABLE d (id BIGINT);\n")
	fs, _ := checkMigrations(dir, nil)
	if len(fs) != 2 {
		t.Fatalf("子目录与大写扩展名的无标注迁移都应被检查: %v", fs)
	}
	// 子目录迁移登记后同样受哈希保护。
	writeFile(t, dir, "sub/000003_c.sql", "-- lingedge:class=expand\nCREATE TABLE c (id BIGINT);\n")
	writeFile(t, dir, "000004_d.SQL", "-- lingedge:class=expand\nCREATE TABLE d (id BIGINT);\n")
	added, err := sealMigrations(dir)
	if err != nil || len(added) != 2 {
		t.Fatalf("seal: %v %v", added, err)
	}
	writeFile(t, dir, "sub/000003_c.sql", "-- lingedge:class=expand\nCREATE TABLE c2 (id BIGINT);\n")
	if fs, _ := checkMigrations(dir, nil); len(fs) != 1 {
		t.Fatalf("改动子目录里的已发布迁移必须失败: %v", fs)
	}
}

// Story 1.6 收紧：标注必须在第一行且取值合法；embed.go 不是迁移。
func TestMigrationsClassAnnotationMustBeValidAndFirstLine(t *testing.T) {
	for name, body := range map[string]string{
		"取值非法":        "-- lingedge:class=contract\nCREATE TABLE c (id BIGINT);\n",
		"不在第一行":       "-- 说明\n-- lingedge:class=expand\nCREATE TABLE c (id BIGINT);\n",
		"旧写法（无等号）":    "-- lingedge:class expand\nCREATE TABLE c (id BIGINT);\n",
		"标注在 SQL 注释里": "SELECT 1; -- lingedge:class=expand\n",
	} {
		dir := sealedDir(t)
		writeFile(t, dir, "000003_c.sql", body)
		if fs, _ := checkMigrations(dir, nil); len(fs) != 1 || !strings.Contains(fs[0].Msg, "第一行") {
			t.Errorf("%s：必须被拒绝: %v", name, fs)
		}
	}
	for _, class := range []string{"reversible", "expand", "irreversible"} {
		dir := sealedDir(t)
		writeFile(t, dir, "000003_c.sql", "-- lingedge:class="+class+"\nCREATE TABLE c (id BIGINT);\n")
		if fs, _ := checkMigrations(dir, nil); len(fs) != 0 {
			t.Errorf("%s 应通过: %v", class, fs)
		}
	}
}

func TestMigrationsEmbedGoIsNotAMigration(t *testing.T) {
	dir := sealedDir(t)
	writeFile(t, dir, "embed.go", "package migrations\n")
	if fs, _ := checkMigrations(dir, nil); len(fs) != 0 {
		t.Fatalf("embed.go 不是迁移，不应要求标注: %v", fs)
	}
	if added, err := sealMigrations(dir); err != nil || len(added) != 0 {
		t.Fatalf("embed.go 不应被登记为已发布迁移: %v %v", added, err)
	}
}
