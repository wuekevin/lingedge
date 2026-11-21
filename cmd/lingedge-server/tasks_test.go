package main

import (
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/taskrt"
)

// requiredRegistered 是「截至当前已完成的 Story，必须已登记」的任务类型（M14：M1 阶段所需类型在其 Story 完成时均已登记）。
// 每个业务 Story 完成时把自己的类型追加到这里（同时在 tasks.go 登记处理函数）；Story 1.14 本身没有内核所需的 OpenAPI TaskType，故为空。
var requiredRegistered = []taskrt.Type{}

// pendingHandlers 登记「TaskType 已在 OpenAPI/taskrt 白名单中、处理函数属后续 Story、当前**有意**未登记」的类型及其归属（Story 1.22b）：
// 这是「仅登记不实现」——不写假处理函数。当前唯一一项：secrets.rewrap 的原语（pkg/secretbox.Box.RewrapAll）与库表仓储（internal/platform/secrets/repository）
// 均已就绪并有真实 MySQL 测试（Story 1.11），worker 任务接线随 Tools 密钥轮换/备份流程的 Story（tools-design §13/§17）交付。
// 一旦某类型被登记（实现了处理函数），测试要求把它从这里移到 requiredRegistered，防止清单陈旧。
var pendingHandlers = []struct {
	Type  taskrt.Type
	Owner string
}{
	{taskrt.TypeSecretsRewrap, "Tools 密钥轮换/备份接线 Story（Story 1.11 遗留，原语与仓储已就绪）"},
}

func TestPendingHandlersAreKnownAndNotYetRegistered(t *testing.T) {
	reg, err := assembleTaskRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pendingHandlers {
		ty, owner := p.Type, p.Owner
		if !taskrt.IsKnown(ty) {
			t.Errorf("待接线类型 %q 不在 OpenAPI TaskType 白名单内", ty)
		}
		if owner == "" {
			t.Errorf("待接线类型 %q 必须写明归属 Story", ty)
		}
		if _, ok := reg.Lookup(ty); ok {
			t.Errorf("类型 %q 已登记处理函数：请把它从 pendingHandlers 移到 requiredRegistered", ty)
		}
	}
}

func TestTaskRegistryAssembly(t *testing.T) {
	reg, err := assembleTaskRegistry()
	if err != nil {
		t.Fatal(err)
	}
	registered := map[taskrt.Type]bool{}
	for _, ty := range reg.Types() {
		registered[ty] = true
		if !taskrt.IsKnown(ty) { // 已登记 ⊆ OpenAPI TaskType（registry_test.go 另行核对 KnownTypes == OpenAPI 枚举）
			t.Errorf("已登记类型 %q 不在 OpenAPI TaskType 白名单内", ty)
		}
		if why := taskrt.ForbiddenTypeReason(ty); why != "" {
			t.Errorf("已登记类型 %q 带执行命令语义: %s", ty, why)
		}
	}
	for _, ty := range requiredRegistered {
		if !registered[ty] {
			t.Errorf("任务类型 %q 的 Story 已完成但尚未登记（M14）", ty)
		}
	}
}

// 未登记类型入队即拒绝：生产装配的注册表不含任何类型，因此任何类型都查不到。
func TestProductionRegistryRejectsUnregistered(t *testing.T) {
	reg, _ := assembleTaskRegistry()
	for _, ty := range append(taskrt.KnownTypes(), "shell.exec", "sh -c", "") {
		if _, ok := reg.Lookup(ty); ok && !contains(requiredRegistered, ty) {
			t.Errorf("类型 %q 不应已登记", ty)
		}
	}
}

func contains(l []taskrt.Type, t taskrt.Type) bool {
	for _, x := range l {
		if x == t {
			return true
		}
	}
	return false
}
