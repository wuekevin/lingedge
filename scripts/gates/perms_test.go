package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/rbac"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func readRepo(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 真实仓库必须通过（正向）。
func TestPermsGateAcceptsRepository(t *testing.T) {
	fs, err := runPerms(repoRoot(t), options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 0 {
		t.Fatalf("权限一致性门禁应通过，实际:\n%v", fs)
	}
	ops := parseOpenAPIPerms(readRepo(t, permsOpenAPIRel))
	if len(ops) < 400 {
		t.Fatalf("openapi 解析出的 operation 数异常: %d", len(ops))
	}
	var withPerms int
	for _, o := range ops {
		if len(o.Perms) > 0 {
			withPerms++
		}
		if o.Scope == "" {
			t.Errorf("%s 没有解析到 x-tenant-scope", o.ID)
		}
	}
	if withPerms < 300 {
		t.Fatalf("带权限的 operation 过少（解析退化？）: %d", withPerms)
	}
}

func hasRule(fs []Finding, rule, sub string) bool {
	for _, f := range fs {
		if f.Rule == rule && strings.Contains(f.Msg, sub) {
			return true
		}
	}
	return false
}

func dropFromRegistry(reg []rbac.Permission, code string) []rbac.Permission {
	var out []rbac.Permission
	for _, p := range reg {
		if p.Code != code {
			out = append(out, p)
		}
	}
	return out
}

// 负向样本：每一种不一致都必须被拦截。
func TestPermsGateRejectsInconsistencies(t *testing.T) {
	oa := readRepo(t, permsOpenAPIRel)
	seed := readRepo(t, permsSeedRel)
	reg := rbac.All()

	t.Run("OpenAPI 引用了不存在的权限串", func(t *testing.T) {
		bad := strings.Replace(oa, "      - user:read\n", "      - user:readd\n", 1)
		if bad == oa {
			t.Fatal("样本构造失败")
		}
		if fs := checkPerms(bad, seed, reg); !hasRule(fs, "perms-openapi", "user:readd") {
			t.Fatalf("未拦截: %v", fs)
		}
	})
	t.Run("OpenAPI 引用的权限只在种子里缺失", func(t *testing.T) {
		bad := strings.Replace(seed, "  ('user:read', 'user', 'read', 'platform', 'auth', '平台用户管理'),\n", "", 1)
		if bad == seed {
			t.Fatal("样本构造失败")
		}
		fs := checkPerms(oa, bad, reg)
		if !hasRule(fs, "perms-seed", "user:read") {
			t.Fatalf("未拦截（注册表有、种子无）: %v", fs)
		}
	})
	t.Run("OpenAPI 引用的权限只在注册表里缺失", func(t *testing.T) {
		fs := checkPerms(oa, seed, dropFromRegistry(reg, "user:read"))
		if !hasRule(fs, "perms-openapi", "user:read") || !hasRule(fs, "perms-registry", "user:read") {
			t.Fatalf("未拦截（OpenAPI 与种子有、注册表无）: %v", fs)
		}
	})
	t.Run("种子的 scope 与注册表不一致", func(t *testing.T) {
		bad := strings.Replace(seed, "'site:read', 'site', 'read', 'both'", "'site:read', 'site', 'read', 'tenant'", 1)
		if bad == seed {
			t.Fatal("样本构造失败")
		}
		if fs := checkPerms(oa, bad, reg); !hasRule(fs, "perms-seed", "site:read") {
			t.Fatalf("未拦截: %v", fs)
		}
	})
	t.Run("platform 路由引用 tenant 权限", func(t *testing.T) {
		bad := strings.Replace(oa, "      - user:read\n", "      - site:write\n", 1) // listUsers 是 platform 路由；site:write 是 tenant 权限
		if fs := checkPerms(bad, seed, reg); !hasRule(fs, "perms-scope", "site:write") {
			t.Fatalf("未拦截: %v", fs)
		}
	})
	t.Run("tenant/either 路由引用 platform 权限", func(t *testing.T) {
		// 把某个 either 路由的权限换成 platform 权限：找 listTasks（either，task:read）。
		i := strings.Index(oa, "operationId: listTasks\n")
		if i < 0 {
			t.Fatal("样本构造失败：找不到 listTasks")
		}
		j := strings.Index(oa[i:], "      - task:read\n")
		if j < 0 {
			t.Fatal("样本构造失败：listTasks 没有 task:read")
		}
		bad := oa[:i+j] + "      - user:read\n" + oa[i+j+len("      - task:read\n"):]
		if fs := checkPerms(bad, seed, reg); !hasRule(fs, "perms-scope", "user:read") {
			t.Fatalf("未拦截: %v", fs)
		}
	})
	t.Run("tenant:impersonate 缺失或 scope 错误", func(t *testing.T) {
		if fs := checkPerms(oa, seed, dropFromRegistry(reg, rbac.Impersonate)); !hasRule(fs, "perms-registry", rbac.Impersonate) {
			t.Fatalf("未拦截缺失: %v", fs)
		}
		mut := append([]rbac.Permission(nil), reg...)
		for i := range mut {
			if mut[i].Code == rbac.Impersonate {
				mut[i].Scope = rbac.ScopeBoth
			}
		}
		if fs := checkPerms(oa, seed, mut); !hasRule(fs, "perms-registry", "scope 必须是 platform") {
			t.Fatalf("未拦截 scope 错误: %v", fs)
		}
	})
	t.Run("注册表重复与 code 不等于 resource:action", func(t *testing.T) {
		dup := append(append([]rbac.Permission(nil), reg...), reg[0])
		if fs := checkPerms(oa, seed, dup); !hasRule(fs, "perms-registry", "重复") {
			t.Fatalf("未拦截重复: %v", fs)
		}
		mut := append([]rbac.Permission(nil), reg...)
		mut[0].Action = mut[0].Action + "x"
		if fs := checkPerms(oa, seed, mut); !hasRule(fs, "perms-registry", "resource:action") {
			t.Fatalf("未拦截 code 不一致: %v", fs)
		}
	})
	t.Run("种子多出注册表没有的权限", func(t *testing.T) {
		bad := strings.Replace(seed, "ON DUPLICATE KEY UPDATE code = code;\n\n-- ---- 内置角色", "  ,('ghost:read', 'ghost', 'read', 'platform', 'auth', 'x')\nON DUPLICATE KEY UPDATE code = code;\n\n-- ---- 内置角色", 1)
		if fs := checkPerms(oa, bad, reg); !hasRule(fs, "perms-registry", "ghost:read") {
			t.Fatalf("未拦截: %v", fs)
		}
	})
	t.Run("公开路由声明权限", func(t *testing.T) {
		i := strings.Index(oa, "operationId: login\n")
		if i < 0 {
			t.Fatal("样本构造失败")
		}
		j := strings.Index(oa[i:], "      x-permissions: []\n")
		if j < 0 {
			t.Fatal("样本构造失败：login 的 x-permissions 不是空列表")
		}
		bad := oa[:i+j] + "      x-permissions:\n      - user:read\n" + oa[i+j+len("      x-permissions: []\n"):]
		if fs := checkPerms(bad, seed, reg); !hasRule(fs, "perms-scope", "公开路由") {
			t.Fatalf("未拦截: %v", fs)
		}
	})
	t.Run("解析为空时不得放行", func(t *testing.T) {
		if fs := checkPerms("openapi: 3.1.0\n", seed, reg); !hasRule(fs, "perms-openapi", "未能从 openapi.yaml 解析") {
			t.Fatalf("OpenAPI 解析为空被放行: %v", fs)
		}
		if fs := checkPerms(oa, "-- empty\n", reg); !hasRule(fs, "perms-seed", "未能从种子迁移解析") {
			t.Fatalf("种子解析为空被放行: %v", fs)
		}
	})
}
