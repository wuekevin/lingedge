package migrationstest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/migrate"
)

func (e *env) cliArgs(extra ...string) []string {
	return append(extra, "--host", e.inst.Host(), "--port", fmt.Sprint(e.inst.Port), "--database", e.dbName, "--user", e.user)
}

func (e *env) cliEnv() []string { return []string{migrate.EnvDBPass + "=" + e.pw} }

// 经 `migrate.Main`（即 lingedge-server migrate 的实现）走完整命令面：plan → up → status → down → 拒绝 → dirty 的退出码。
func TestCLIEndToEndAgainstRealMySQL(t *testing.T) {
	e := newEnv(t)
	fsys := fixtureFS(t, "good", "extend/contract")
	run := func(env []string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := migrate.Main(args, &out, &errb, env, fsys)
		return code, out.String(), errb.String()
	}

	// plan（连库）：待执行含不可逆。
	code, out, errs := run(e.cliEnv(), e.cliArgs("plan")...)
	if code != 0 {
		t.Fatalf("plan: %d %s", code, errs)
	}
	var plan migrate.Plan
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("plan 输出不是 JSON: %v\n%s", err, out)
	}
	if plan.Mode != "pending" || plan.Count != 6 || !plan.HasIrreversible || plan.Migrations[5].Class != migrate.ClassIrreversible || plan.Migrations[5].HasDown {
		t.Fatalf("plan: %+v", plan)
	}
	// 含不可逆迁移的 up 未确认 → 退出码 9，一个都不应用。
	code, _, errs = run(e.cliEnv(), e.cliArgs("up")...)
	if code != migrate.ExitIrreversible || !strings.Contains(errs, "--accept-irreversible") {
		t.Fatalf("up 未确认不可逆: code=%d %s", code, errs)
	}
	// 只升到 5。
	if code, out, errs = run(e.cliEnv(), e.cliArgs("up", "--to", "5")...); code != 0 || !strings.Contains(out, "000005_widget_note_column") {
		t.Fatalf("up --to 5: %d %s %s", code, out, errs)
	}
	// status（文本与 JSON）。
	code, out, _ = run(e.cliEnv(), e.cliArgs("status")...)
	if code != 0 || !strings.Contains(out, "数据库版本 000005") || !strings.Contains(out, "待执行 1") {
		t.Fatalf("status: %d %s", code, out)
	}
	code, out, _ = run(e.cliEnv(), e.cliArgs("status", "--json")...)
	var st migrate.Status
	if err := json.Unmarshal([]byte(out), &st); err != nil || code != 0 || st.DBVersion != 5 || st.Pending != 1 || len(st.Migrations) != 6 {
		t.Fatalf("status --json: %d %v %+v", code, err, st)
	}
	// down 一步（000005 reversible）成功；再 down（000004 expand）被拒。
	if code, out, errs = run(e.cliEnv(), e.cliArgs("down")...); code != 0 || !strings.Contains(out, "回退") {
		t.Fatalf("down: %d %s %s", code, out, errs)
	}
	if code, _, errs = run(e.cliEnv(), e.cliArgs("down")...); code != migrate.ExitFailure || !strings.Contains(errs, "expand") {
		t.Fatalf("expand 回退应失败: %d %s", code, errs)
	}
	// 确认后升到最新。
	if code, _, errs = run(e.cliEnv(), e.cliArgs("up", "--accept-irreversible")...); code != 0 {
		t.Fatalf("up --accept-irreversible: %d %s", code, errs)
	}
	wantVersions(t, e, "1", "2", "3", "4", "5", "6")

	// 口令错误：失败、且错误信息不泄露口令。
	code, _, errs = run([]string{migrate.EnvDBPass + "=wrong-" + e.pw}, e.cliArgs("status")...)
	if code != migrate.ExitFailure || strings.Contains(errs, e.pw) {
		t.Fatalf("口令错误: code=%d err=%s", code, errs)
	}
}

// 口令文件：权限过宽被拒；0600 通过。
func TestCLIPasswordFile(t *testing.T) {
	e := newEnv(t)
	fsys := fixtureFS(t, "good")
	dir := t.TempDir()
	pwFile := filepath.Join(dir, "pw")
	if err := os.WriteFile(pwFile, []byte(e.pw+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pwFile, 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := migrate.Main(e.cliArgs("status"), &out, &errb, []string{migrate.EnvDBPassFile + "=" + pwFile}, fsys)
	if code != migrate.ExitFailure || !strings.Contains(errb.String(), "权限过宽") {
		t.Fatalf("权限过宽的口令文件必须被拒绝: %d %s", code, errb.String())
	}
	if err := os.Chmod(pwFile, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	if code := migrate.Main(e.cliArgs("status"), &out, &errb, []string{migrate.EnvDBPassFile + "=" + pwFile}, fsys); code != 0 {
		t.Fatalf("0600 口令文件应通过: %d %s", code, errb.String())
	}
}

// 真实的 lingedge-server 二进制（生产构建，嵌入 migrations/）：migrate 子命令的离线与连库路径、退出码。
func TestLingedgeServerBinaryMigrateSubcommand(t *testing.T) {
	e := newEnv(t)
	bin := buildHelper(t, "./cmd/lingedge-server")
	run := func(env []string, args ...string) (int, string, string) {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), env...)
		var o, er bytes.Buffer
		cmd.Stdout, cmd.Stderr = &o, &er
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return code, o.String(), er.String()
	}
	if code, out, _ := run(nil, "--version"); code != 0 || !strings.Contains(out, "lingedge-server") {
		t.Fatalf("--version 行为不得改变: %d %s", code, out)
	}
	if code, out, errs := run(nil, "migrate", "lint"); code != 0 || !strings.Contains(out, "lint 通过") {
		t.Fatalf("lint: %d %s %s", code, out, errs)
	}
	code, out, errs := run(nil, "migrate", "plan")
	var p migrate.Plan
	if code != 0 || json.Unmarshal([]byte(out), &p) != nil || p.Mode != "all" {
		t.Fatalf("离线 plan: %d %s %s", code, out, errs)
	}
	// ADR-026 §4：非交互且无任何口令来源 → 退出码 3（前置条件），并说明环境变量与 credential init。
	if code, _, errs := run(nil, "migrate", "up", "--host", e.inst.Host(), "--database", e.dbName); code != migrate.ExitPrecondition || !strings.Contains(errs, migrate.EnvDBPass) {
		t.Fatalf("缺口令: %d %s", code, errs)
	}
	// 嵌入的迁移（当前尚无，1.7 起落库）：连库 up/status 都应成功且不改动业务表。
	env := []string{migrate.EnvDBPass + "=" + e.pw}
	args := func(sub string) []string {
		return append([]string{"migrate", sub}, e.cliArgs()...)
	}
	if code, out, errs := run(env, args("up")...); code != 0 {
		t.Fatalf("up: %d %s %s", code, out, errs)
	}
	if code, out, errs := run(env, args("status")...); code != 0 || !strings.Contains(out, "待执行 0") {
		t.Fatalf("status: %d %s %s", code, out, errs)
	}
	if code, _, _ := run(nil, "migrate", "bogus"); code != migrate.ExitUsage {
		t.Fatalf("未知子命令退出码 = %d", code)
	}
}

// --config：host/port/name 取自 server.yaml，迁移账号口令来自环境变量（LINGEDGE_MIGRATE_* 不被配置加载器当作未知覆盖拒绝）；
// 只设置 LINGEDGE_DATABASE__* 环境变量（无 --config）同样可用。
func TestCLIConfigFileAndDatabaseEnv(t *testing.T) {
	e := newEnv(t)
	fsys := fixtureFS(t, "good")
	dir := t.TempDir()
	yaml := fmt.Sprintf("server:\n  env: test\ndatabase:\n  host: %s\n  port: %d\n  name: %s\n  user: unused\n", e.inst.Host(), e.inst.Port, e.dbName)
	cfgFile := filepath.Join(dir, "server.yaml")
	if err := os.WriteFile(cfgFile, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(env []string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := migrate.Main(args, &out, &errb, env, fsys)
		return code, out.String(), errb.String()
	}
	env := []string{migrate.EnvDBPass + "=" + e.pw, migrate.EnvUser + "=" + e.user}
	if code, out, errs := run(env, "up", "--config", cfgFile); code != 0 {
		t.Fatalf("--config up: %d %s %s", code, out, errs)
	}
	wantVersions(t, e, "1", "2", "3", "4", "5")
	if code, out, errs := run(env, "status", "--config", cfgFile); code != 0 || !strings.Contains(out, "数据库版本 000005") {
		t.Fatalf("--config status: %d %s %s", code, out, errs)
	}
	// 无 --config：仅环境变量。
	envOnly := append([]string{
		"LINGEDGE_DATABASE__HOST=" + e.inst.Host(), fmt.Sprintf("LINGEDGE_DATABASE__PORT=%d", e.inst.Port), "LINGEDGE_DATABASE__NAME=" + e.dbName,
	}, env...)
	if code, out, errs := run(envOnly, "status"); code != 0 || !strings.Contains(out, "数据库版本 000005") {
		t.Fatalf("仅环境变量 status: %d %s %s", code, out, errs)
	}
	// 非 LINGEDGE_MIGRATE_* 的未知 LINGEDGE_ 变量仍被配置加载器拒绝（--config 路径）。
	if code, _, errs := run(append([]string{"LINGEDGE_BOGUS__KEY=1"}, env...), "status", "--config", cfgFile); code != migrate.ExitFailure || !strings.Contains(errs, "LINGEDGE_BOGUS__KEY") {
		t.Fatalf("未知覆盖变量应被拒绝: %d %s", code, errs)
	}
}

// down 的选项经 CLI 解析到真实库：--steps、--to（保留）、--all、显式 --to 0（= 全部，而不是退化成一步）。
func TestCLIDownOptions(t *testing.T) {
	e := newEnv(t)
	fsys := fixtureFS(t, "reversible")
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := migrate.Main(args, &out, &errb, e.cliEnv(), fsys)
		return code, out.String(), errb.String()
	}
	up := func() {
		t.Helper()
		if code, out, errs := run(e.cliArgs("up")...); code != 0 {
			t.Fatalf("up: %d %s %s", code, out, errs)
		}
	}
	up()
	if code, _, errs := run(e.cliArgs("down", "--steps", "2")...); code != 0 {
		t.Fatalf("down --steps 2: %d %s", code, errs)
	}
	wantVersions(t, e, "1", "2")
	up()
	if code, _, errs := run(e.cliArgs("down", "--to", "1")...); code != 0 {
		t.Fatalf("down --to 1: %d %s", code, errs)
	}
	wantVersions(t, e, "1")
	up()
	if code, _, errs := run(e.cliArgs("down", "--to", "0")...); code != 0 {
		t.Fatalf("down --to 0: %d %s", code, errs)
	}
	wantVersions(t, e) // 显式 --to 0 = 回退全部
	up()
	if code, out, errs := run(e.cliArgs("down", "--all", "--json")...); code != 0 || !strings.Contains(out, `"to_version": 0`) {
		t.Fatalf("down --all: %d %s %s", code, out, errs)
	}
	wantVersions(t, e)
	// 失败时 --json 仍输出已成功的部分（这里 down 无可回退 → 成功且为空）。
	if code, _, _ := run(e.cliArgs("down", "--steps", "1", "--all")...); code != migrate.ExitUsage {
		t.Fatalf("选项互斥应是用法错误: %d", code)
	}
}

// up 中途失败时仍输出已经成功应用的迁移（调用方据此知道库停在了哪里），退出码为失败。
func TestCLIUpFailureStillReportsAppliedMigrations(t *testing.T) {
	e := newEnv(t)
	var out, errb bytes.Buffer
	code := migrate.Main(e.cliArgs("up", "--json"), &out, &errb, e.cliEnv(), fixtureFS(t, "dml-atomic"))
	if code != migrate.ExitFailure {
		t.Fatalf("退出码 = %d，stderr: %s", code, errb.String())
	}
	var res migrate.UpResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || len(res.Applied) != 1 || res.Applied[0].Version != 1 || res.ToVersion != 1 {
		t.Fatalf("失败时应输出已应用的 000001: %v %+v\n%s", err, res, out.String())
	}
	if !strings.Contains(errb.String(), "1048") || !strings.Contains(errb.String(), "dirty") {
		t.Fatalf("stderr 应含真实错误与 dirty 说明: %s", errb.String())
	}
}
