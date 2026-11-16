package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo 在临时目录建一个仓库，放入真实的 openapi.yaml 与 README，提交并打 contract-v1 标签，然后切换 cwd 进去。
func gitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	root := repoRoot(t)
	for _, rel := range []string{"docs/api/openapi.yaml", "docs/api/README.md"} {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, rel), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(repo, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	g := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	g("init", "-q")
	g("add", ".")
	g("commit", "-q", "-m", "base")
	g("tag", "-a", "contract-v1", "-m", "contract-v1")
	old, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	return repo
}

func capture(t *testing.T, f func() int) (code int) {
	t.Helper()
	// 子命令把诊断写 stderr/stdout；测试只关心退出码，输出由 go test -v 呈现。
	return f()
}

// TestOpenAPIBreakingAgainstGitTag 端到端：真实 git 标签基线 + 真实 oasdiff。
func TestOpenAPIBreakingAgainstGitTag(t *testing.T) {
	sev, _ := filepath.Abs(filepath.Join(repoRoot(t), "scripts", "contract", "data", "oasdiff-severity-levels.txt"))
	oas := strings.Join(toolCmd("OASDIFF", defaultOasdiff), " ")
	run := func(extra ...string) int {
		return capture(t, func() int {
			return cmdOpenAPIBreaking(append([]string{"-severity-levels", sev, "-oasdiff", oas}, extra...))
		})
	}
	repo := gitRepo(t)
	spec := filepath.Join(repo, "docs", "api", "openapi.yaml")
	readme := filepath.Join(repo, "docs", "api", "README.md")
	orig, _ := os.ReadFile(spec)
	origReadme, _ := os.ReadFile(readme)

	if code := run(); code != 0 {
		t.Fatalf("与标签一致应通过，退出码 %d", code)
	}

	// 向后兼容新增：TaskType 新增枚举值（AC9 的非破坏形态）。
	added := strings.Replace(string(orig), "      - deployment.retry\n", "      - deployment.retry\n      - added.in_test\n", 1)
	if added == string(orig) {
		t.Fatal("样例替换失败")
	}
	_ = os.WriteFile(spec, []byte(added), 0o644)
	if code := run(); code != 0 {
		t.Fatalf("新增 TaskType 枚举值应判向后兼容，退出码 %d", code)
	}

	// 破坏性：改 operationId（oasdiff 看不到，由契约扩展检查抓住）。
	renamed := strings.Replace(string(orig), "operationId: listSites\n", "operationId: listSitesRenamed\n", 1)
	_ = os.WriteFile(spec, []byte(renamed), 0o644)
	if code := run(); code == 0 {
		t.Fatal("operationId 改名无说明时必须失败")
	}

	// 破坏性：删除 operation 路径下的一个 200 响应字段由 oasdiff 抓住 —— 删除 Error.code 的 required 项。
	tightened := strings.Replace(string(orig), "      required:\n      - code\n      - message\n      - request_id\n", "      required:\n      - code\n      - message\n", 1)
	if tightened == string(orig) {
		t.Fatal("样例替换失败")
	}
	_ = os.WriteFile(spec, []byte(tightened), 0o644)
	if code := run(); code == 0 {
		t.Fatal("响应必填字段被放宽（request_id 变可选）无说明时必须失败")
	}

	// 放行：有效变更说明（位于 docs/bmad/contract-changes/，逐项列出本次检出的变更类型）。
	noteDirPath := filepath.Join(repo, "docs", "bmad", "contract-changes")
	_ = os.MkdirAll(noteDirPath, 0o755)
	ids := map[string]bool{}
	for _, ch := range oasdiffBreaking(t, orig, []byte(tightened)) {
		ids[ch.ID] = true
	}
	list := ""
	for id := range ids {
		list += id + " "
	}
	note := filepath.Join(noteDirPath, "cc.md")
	_ = os.WriteFile(note, []byte("- **影响范围**：Error.request_id\n- **是否破坏**：是（"+list+"）\n- **关联 ADR**：ADR-017\n"), 0o644)
	if code := run("-note", note); code != 0 {
		t.Fatalf("有效变更说明应放行，退出码 %d", code)
	}
	badNote := filepath.Join(noteDirPath, "bad.md")
	_ = os.WriteFile(badNote, []byte("- **是否破坏**：否\n"), 0o644)
	if code := run("-note", badNote); code == 0 {
		t.Fatal("无效变更说明不得放行")
	}
	// 说明不在规定目录。
	stray := filepath.Join(t.TempDir(), "cc.md")
	_ = os.WriteFile(stray, []byte("- **影响范围**：x\n- **是否破坏**：是 "+list+"\n- **关联 ADR**：ADR-017\n"), 0o644)
	if code := run("-note", stray); code == 0 {
		t.Fatal("目录之外的说明不得放行")
	}

	// 错误码：删除 README 错误码索引中的一项。
	_ = os.WriteFile(spec, orig, 0o644)
	_ = os.WriteFile(readme, []byte(strings.Replace(string(origReadme), "| `site.domain_conflict` | 409 | 域名已被其他活动站点占用 |\n", "", 1)), 0o644)
	if string(origReadme) == "" || run() == 0 {
		t.Fatal("删除错误码无说明时必须失败")
	}
	_ = os.WriteFile(readme, origReadme, 0o644)

	// 基线标签不存在：必须失败而不是静默跳过。
	if code := run("-base-ref", "contract-v999"); code == 0 {
		t.Fatal("基线标签不存在必须失败")
	}
}

// TestMigrationsCheckAgainstGitTag：清单相对标签基线只追加（端到端）。
func TestMigrationsCheckAgainstGitTag(t *testing.T) {
	repo := gitRepo(t)
	mdir := filepath.Join(repo, "migrations")
	_ = os.WriteFile(filepath.Join(mdir, "CHECKSUMS"), []byte("# h\n"), 0o644)
	_ = os.WriteFile(filepath.Join(mdir, "000001_a.sql"), []byte(mig1), 0o644)
	if _, err := sealMigrations(mdir); err != nil {
		t.Fatal(err)
	}
	g := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	g("add", ".")
	g("commit", "-q", "-m", "m")
	g("tag", "-a", "contract-v2", "-m", "v2")
	if code := cmdMigrationsCheck([]string{"-base-ref", "contract-v2"}); code != 0 {
		t.Fatalf("与基线一致应通过: %d", code)
	}
	// 改动已发布迁移。
	_ = os.WriteFile(filepath.Join(mdir, "000001_a.sql"), []byte(mig1+"-- x\n"), 0o644)
	if code := cmdMigrationsCheck([]string{"-base-ref", "contract-v2"}); code == 0 {
		t.Fatal("改动已发布迁移必须失败")
	}
	_ = os.WriteFile(filepath.Join(mdir, "000001_a.sql"), []byte(mig1), 0o644)
	// 基线（contract-v1）里没有清单文件：视为无基线条目，应通过。
	if code := cmdMigrationsCheck([]string{"-base-ref", "contract-v1"}); code != 0 {
		t.Fatalf("基线无清单应通过: %d", code)
	}
	// 基线 ref 不存在：失败。
	if code := cmdMigrationsCheck([]string{"-base-ref", "contract-v999"}); code == 0 {
		t.Fatal("基线 ref 不存在必须失败")
	}
}

func gitIn(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// 门禁策略文件（oasdiff 严重级别、buf.yaml）相对基线被改动，等同放宽检测，必须判为破坏并要求说明。
func TestOpenAPIBreakingPolicyFileChanged(t *testing.T) {
	oas := strings.Join(toolCmd("OASDIFF", defaultOasdiff), " ")
	repo := gitRepo(t)
	sevRel := "scripts/contract/data/oasdiff-severity-levels.txt"
	real, _ := os.ReadFile(filepath.Join(repoRoot(t), sevRel))
	if err := os.MkdirAll(filepath.Join(repo, filepath.Dir(sevRel)), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(repo, sevRel), real, 0o644)
	_ = os.MkdirAll(filepath.Join(repo, "proto"), 0o755)
	_ = os.WriteFile(filepath.Join(repo, "proto", "buf.yaml"), []byte("version: v2\n"), 0o644)
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "policy")
	gitIn(t, repo, "tag", "-f", "-a", "contract-v1", "-m", "v1")
	run := func() int { return cmdOpenAPIBreaking([]string{"-severity-levels", sevRel, "-oasdiff", oas}) }
	if code := run(); code != 0 {
		t.Fatalf("策略文件未改动应通过: %d", code)
	}
	_ = os.WriteFile(filepath.Join(repo, sevRel), append(real, []byte("response-optional-property-removed info\n")...), 0o644)
	if code := run(); code == 0 {
		t.Fatal("放宽 oasdiff 严重级别（策略文件改动）无说明时必须失败")
	}
	_ = os.WriteFile(filepath.Join(repo, sevRel), real, 0o644)
	_ = os.WriteFile(filepath.Join(repo, "proto", "buf.yaml"), []byte("version: v2\nbreaking:\n  use: [PACKAGE]\n"), 0o644)
	if code := run(); code == 0 {
		t.Fatal("改动 buf.yaml 策略无说明时必须失败")
	}
}

// 基线中没有错误码索引（README 缺失）时必须失败，而不是跳过错误码比较。
func TestOpenAPIBreakingBaseReadmeMissing(t *testing.T) {
	oas := strings.Join(toolCmd("OASDIFF", defaultOasdiff), " ")
	repo := gitRepo(t)
	gitIn(t, repo, "rm", "-q", "docs/api/README.md")
	gitIn(t, repo, "commit", "-q", "-m", "drop readme")
	gitIn(t, repo, "tag", "-f", "-a", "contract-v1", "-m", "v1")
	_ = os.WriteFile(filepath.Join(repo, "docs", "api", "README.md"), []byte("x\n"), 0o644)
	if code := cmdOpenAPIBreaking([]string{"-oasdiff", oas, "-severity-levels", ""}); code == 0 {
		t.Fatal("基线缺 README 必须失败")
	}
}
