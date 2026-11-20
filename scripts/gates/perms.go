package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/platform/rbac"
)

// 权限一致性门禁（Story 1.18 AC1、ADR-018 §8「CI 检查 x-permissions 均存在于种子」）：
//
//	① docs/api/openapi.yaml 引用的每个权限串必须存在于代码注册表（internal/platform/rbac）；
//	② 代码注册表与种子迁移（migrations/000004_seed_builtin.sql）的 permissions 行逐项一致（权限串、scope、module），双向无多无少——
//	   因此 OpenAPI 引用的权限串「存在且已种子」；
//	③ scope 相容：x-tenant-scope=platform 的 operation 只能引用 scope 为 platform/both 的权限；tenant/either 的 operation 只能引用 tenant/both
//	   （平台主体经 acting-as 代租户时只认 tenant/both 权限，ADR-018 §10——引用 platform 权限的 tenant/either 路由将永远无法被 acting-as 授权）；
//	   公开路由不得声明权限；
//	④ 权限字典含 tenant:impersonate 且 scope=platform；注册表的 code 必须等于 resource:action、无重复。
//
// 纯文本解析（仅标准库）：openapi.yaml 的 operation 缩进固定（operationId/x-permissions/x-tenant-scope 位于 6 空格）。
// 门禁自测（perms_test.go）含负向样本：缺注册、缺种子、scope 不符、未知权限串、重复、公开路由带权限都必须被拦截。

const (
	permsOpenAPIRel = "docs/api/openapi.yaml"
	permsSeedRel    = "migrations/000004_seed_builtin.sql"
)

type opPerms struct {
	ID    string
	Scope string
	Perms []string
	Line  int
}

var (
	reOpID     = regexp.MustCompile(`^      operationId: (\S+)\s*$`)
	reOpScope  = regexp.MustCompile(`^      x-tenant-scope: (\S+)\s*$`)
	reOpPermsK = regexp.MustCompile(`^      x-permissions:\s*(\[\s*\])?\s*$`)
	reOpPermsV = regexp.MustCompile(`^      - (\S+)\s*$`)
	reSeedRow  = regexp.MustCompile(`\('([a-z0-9:_-]+)', '([^']+)', '([^']+)', '(platform|tenant|both)', '([a-z0-9_]+)', '[^']*'\)`)
)

// parseOpenAPIPerms 提取每个 operation 的 x-tenant-scope 与 x-permissions。
func parseOpenAPIPerms(doc string) []opPerms {
	var ops []opPerms
	var cur *opPerms
	inPerms := false
	for i, l := range strings.Split(doc, "\n") {
		if m := reOpID.FindStringSubmatch(l); m != nil {
			ops = append(ops, opPerms{ID: m[1], Line: i + 1})
			cur = &ops[len(ops)-1]
			inPerms = false
			continue
		}
		if cur == nil {
			continue
		}
		if reOpPermsK.MatchString(l) {
			inPerms = !strings.Contains(l, "[")
			continue
		}
		if inPerms {
			if m := reOpPermsV.FindStringSubmatch(l); m != nil {
				cur.Perms = append(cur.Perms, m[1])
				continue
			}
			inPerms = false
		}
		if m := reOpScope.FindStringSubmatch(l); m != nil {
			cur.Scope = m[1]
		}
	}
	return ops
}

type seedPerm struct{ Code, Resource, Action, Scope, Module string }

func parseSeedPerms(sql string) []seedPerm {
	var out []seedPerm
	for _, m := range reSeedRow.FindAllStringSubmatch(sql, -1) {
		out = append(out, seedPerm{Code: m[1], Resource: m[2], Action: m[3], Scope: m[4], Module: m[5]})
	}
	return out
}

// checkPerms 是门禁的纯函数核心（自测直接喂入变异后的输入）。
func checkPerms(openapi, seedSQL string, reg []rbac.Permission) []Finding {
	var out []Finding
	add := func(file string, line int, rule, f string, a ...any) {
		out = append(out, Finding{File: file, Line: line, Rule: rule, Msg: fmt.Sprintf(f, a...)})
	}
	byCode := map[string]rbac.Permission{}
	for _, p := range reg {
		if _, dup := byCode[p.Code]; dup {
			add("internal/platform/rbac", 0, "perms-registry", "注册表权限串重复: %s", p.Code)
		}
		byCode[p.Code] = p
		if p.Code != p.Resource+":"+p.Action {
			add("internal/platform/rbac", 0, "perms-registry", "%s: code 必须等于 resource:action（%s:%s）", p.Code, p.Resource, p.Action)
		}
		switch p.Scope {
		case rbac.ScopePlatform, rbac.ScopeTenant, rbac.ScopeBoth:
		default:
			add("internal/platform/rbac", 0, "perms-registry", "%s: 非法 scope %q", p.Code, p.Scope)
		}
	}
	if imp, ok := byCode[rbac.Impersonate]; !ok {
		add("internal/platform/rbac", 0, "perms-registry", "权限字典缺少 %s（ADR-018 §10）", rbac.Impersonate)
	} else if imp.Scope != rbac.ScopePlatform {
		add("internal/platform/rbac", 0, "perms-registry", "%s 的 scope 必须是 platform，实际 %s", rbac.Impersonate, imp.Scope)
	}

	// ② 注册表 ⇄ 种子。
	seeded := map[string]seedPerm{}
	for _, s := range parseSeedPerms(seedSQL) {
		if _, dup := seeded[s.Code]; dup {
			add(permsSeedRel, 0, "perms-seed", "种子中权限串重复: %s", s.Code)
		}
		seeded[s.Code] = s
	}
	if len(seeded) == 0 {
		add(permsSeedRel, 0, "perms-seed", "未能从种子迁移解析出任何权限行（格式变化？门禁不能在解析为空时放行）")
	}
	for code, p := range byCode {
		s, ok := seeded[code]
		switch {
		case !ok:
			add(permsSeedRel, 0, "perms-seed", "注册表中的 %s 没有种子（permissions 表缺行）", code)
		case s.Scope != string(p.Scope) || s.Module != p.Module || s.Resource != p.Resource || s.Action != p.Action:
			add(permsSeedRel, 0, "perms-seed", "%s 与种子不一致: 注册表 scope=%s module=%s，种子 scope=%s module=%s", code, p.Scope, p.Module, s.Scope, s.Module)
		}
	}
	for code := range seeded {
		if _, ok := byCode[code]; !ok {
			add("internal/platform/rbac", 0, "perms-registry", "种子中的 %s 不在代码注册表（新增权限须同步三处：OpenAPI、种子、注册表）", code)
		}
	}

	// ① ③ OpenAPI。
	ops := parseOpenAPIPerms(openapi)
	if len(ops) == 0 {
		add(permsOpenAPIRel, 0, "perms-openapi", "未能从 openapi.yaml 解析出任何 operation（格式变化？门禁不能在解析为空时放行）")
	}
	for _, op := range ops {
		if op.Scope == "public" && len(op.Perms) > 0 {
			add(permsOpenAPIRel, op.Line, "perms-scope", "%s: 公开路由（x-tenant-scope: public）不得声明权限 %v", op.ID, op.Perms)
		}
		for _, perm := range op.Perms {
			p, ok := byCode[perm]
			if !ok {
				add(permsOpenAPIRel, op.Line, "perms-openapi", "%s 引用的权限串 %q 不在代码注册表/种子中", op.ID, perm)
				continue
			}
			switch op.Scope {
			case "platform":
				if p.Scope == rbac.ScopeTenant {
					add(permsOpenAPIRel, op.Line, "perms-scope", "%s（platform 路由）引用了 scope=tenant 的权限 %s：平台角色不持有，应为 platform/both", op.ID, perm)
				}
			case "tenant", "either":
				if p.Scope == rbac.ScopePlatform {
					add(permsOpenAPIRel, op.Line, "perms-scope", "%s（%s 路由）引用了 scope=platform 的权限 %s：acting-as 只认 tenant/both 权限（ADR-018 §10），应为 tenant/both", op.ID, op.Scope, perm)
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Msg < out[j].Msg })
	return out
}

func runPerms(root string, _ options) ([]Finding, error) {
	oa, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(permsOpenAPIRel)))
	if err != nil {
		return nil, err
	}
	seed, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(permsSeedRel)))
	if err != nil {
		return nil, err
	}
	return checkPerms(string(oa), string(seed), rbac.All()), nil
}
