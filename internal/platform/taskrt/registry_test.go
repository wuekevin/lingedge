package taskrt_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/httpx/contracttest"
	"github.com/wuekevin/lingedge/internal/platform/taskrt"
)

func nopHandler(context.Context, *taskrt.Run) (taskrt.Result, error) { return taskrt.Result{}, nil }

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, rel)
		}
		p := filepath.Dir(dir)
		if p == dir {
			t.Fatal("找不到仓库根")
		}
		dir = p
	}
}

// AC7（M14）：KnownTypes 与 OpenAPI TaskType 枚举逐项一致（唯一事实源是 openapi.yaml）；已登记 ⊆ KnownTypes 由 Register 强制。
func TestKnownTypesEqualOpenAPITaskType(t *testing.T) {
	spec, err := contracttest.Load(repoFile(t, "docs/api/openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sch := spec.Doc.Components.Schemas["TaskType"]
	if sch == nil || sch.Value == nil {
		t.Fatal("openapi 缺少 TaskType")
	}
	var api []string
	for _, v := range sch.Value.Enum {
		api = append(api, v.(string))
	}
	var code []string
	for _, ty := range taskrt.KnownTypes() {
		code = append(code, string(ty))
	}
	sort.Strings(api)
	if len(api) != 25 {
		t.Errorf("OpenAPI TaskType 应为 25 项，实际 %d", len(api))
	}
	if len(api) != len(code) {
		t.Fatalf("KnownTypes(%d) 与 OpenAPI TaskType(%d) 数量不一致\nAPI:  %v\nCode: %v", len(code), len(api), api, code)
	}
	for i := range api {
		if api[i] != code[i] {
			t.Errorf("第 %d 项不一致：OpenAPI=%s 代码=%s", i, api[i], code[i])
		}
	}
}

// 迁移 000002 的 ck_tasks_type CHECK 取值同样须与 KnownTypes 一致（新增类型走 expand：先放宽 CHECK）。
func TestKnownTypesEqualMigrationCheck(t *testing.T) {
	b, err := os.ReadFile(repoFile(t, "migrations/000002_platform_core.sql"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)ck_tasks_type CHECK \(type IN \((.*?)\)\)`).FindSubmatch(b)
	if m == nil {
		t.Fatal("找不到 ck_tasks_type")
	}
	var mig []string
	for _, s := range regexp.MustCompile(`'([a-z_.]+)'`).FindAllSubmatch(m[1], -1) {
		mig = append(mig, string(s[1]))
	}
	sort.Strings(mig)
	var code []string
	for _, ty := range taskrt.KnownTypes() {
		code = append(code, string(ty))
	}
	if len(mig) != len(code) {
		t.Fatalf("迁移 CHECK(%d) 与 KnownTypes(%d) 不一致", len(mig), len(code))
	}
	for i := range mig {
		if mig[i] != code[i] {
			t.Errorf("第 %d 项：迁移=%s 代码=%s", i, mig[i], code[i])
		}
	}
}

// AC1：白名单。已知类型可登记；未知/命令类/格式非法的类型一律拒绝；重复、空处理函数拒绝。
func TestRegistryWhitelist(t *testing.T) {
	r := taskrt.NewRegistry()
	if err := r.Register(taskrt.Spec{Type: taskrt.TypeRetentionCleanup, Handler: nopHandler}); err != nil {
		t.Fatalf("已知类型应可登记: %v", err)
	}
	s, ok := r.Lookup(taskrt.TypeRetentionCleanup)
	if !ok || s.MaxAttempts != taskrt.DefaultMaxAttempts || s.Priority != taskrt.DefaultPriority {
		t.Fatalf("默认值未填充: %+v", s)
	}
	bad := []taskrt.Type{
		"shell.exec", "exec.command", "system.exec", "sh -c", "bash", "os.command", "script.run", "cmd.run", "node.run_command",
		"unknown.type", "cache.purge_url ", "Cache.Purge_URL", "", "deployment.publish; rm -rf /",
	}
	for _, ty := range bad {
		err := r.Register(taskrt.Spec{Type: ty, Handler: nopHandler})
		if !errors.Is(err, taskrt.ErrTypeNotRegistered) {
			t.Errorf("类型 %q 应被拒绝，得到 %v", ty, err)
		}
		if _, ok := r.Lookup(ty); ok {
			t.Errorf("被拒绝的类型 %q 不应可查到", ty)
		}
	}
	if err := r.Register(taskrt.Spec{Type: taskrt.TypeRetentionCleanup, Handler: nopHandler}); err == nil {
		t.Error("重复登记应失败")
	}
	if err := r.Register(taskrt.Spec{Type: taskrt.TypeCertRenew}); err == nil {
		t.Error("处理函数为空应失败")
	}
	if got := r.Types(); len(got) != 1 || got[0] != taskrt.TypeRetentionCleanup {
		t.Errorf("Types=%v", got)
	}
}

// 纵深防御：即使有人把命令类类型误加进白名单，名称检查也会拦住；且 25 个已知类型自身都不含命令语义。
func TestKnownTypesHaveNoCommandSemantics(t *testing.T) {
	for _, ty := range taskrt.KnownTypes() {
		if why := taskrt.ForbiddenTypeReason(ty); why != "" {
			t.Errorf("已知类型 %q 带命令语义: %s", ty, why)
		}
	}
	for _, ty := range []taskrt.Type{"shell.exec", "foo.run", "node.command", "x.sh", "a.bash_cmd", "job.eval"} {
		if taskrt.ForbiddenTypeReason(ty) == "" {
			t.Errorf("%q 应被判定为命令类", ty)
		}
	}
}

func TestStatusValidity(t *testing.T) {
	for _, s := range []taskrt.Status{taskrt.StatusPending, taskrt.StatusRunning, taskrt.StatusSuccess, taskrt.StatusPartial, taskrt.StatusFailed, taskrt.StatusCanceled} {
		if !s.Valid() {
			t.Errorf("%s 应合法", s)
		}
	}
	if taskrt.Status("done").Valid() || taskrt.StatusPending.Terminal() || taskrt.StatusRunning.Terminal() || !taskrt.StatusPartial.Terminal() {
		t.Error("状态判定错误")
	}
}
