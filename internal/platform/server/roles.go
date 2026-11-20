package server

import (
	"fmt"
	"strings"
)

// Role 是 lingedge-server 的运行角色（architecture §5.4）。
type Role string

// 四个角色。api=REST（含 /healthz）；grpc=Node mTLS :9443 与 Enroll :9444（实现于 Story 3.13）；
// scheduler=周期任务触发（实现于 Story 1.15）；worker=消费 tasks 表（实现于 Story 1.14）。
const (
	RoleAPI       Role = "api"
	RoleGRPC      Role = "grpc"
	RoleScheduler Role = "scheduler"
	RoleWorker    Role = "worker"
)

// AllRoles 是全部角色（默认全开），顺序固定，也是装配与日志的顺序。
var AllRoles = []Role{RoleAPI, RoleGRPC, RoleScheduler, RoleWorker}

// RoleSet 是一组已校验、去重的角色。
type RoleSet struct{ set map[Role]bool }

// DefaultRoles 返回全开的角色集合。
func DefaultRoles() RoleSet {
	rs := RoleSet{set: map[Role]bool{}}
	for _, r := range AllRoles {
		rs.set[r] = true
	}
	return rs
}

// ParseRoles 解析 `--roles` 的取值（逗号分隔）。空串表示默认全开；出现未知角色、空项（如 "api,,worker"）或整体为空白，一律拒绝。
// 重复角色（"api,api"）视为用户笔误而拒绝，避免装配出歧义。
func ParseRoles(s string) (RoleSet, error) {
	if s == "" {
		return DefaultRoles(), nil
	}
	rs := RoleSet{set: map[Role]bool{}}
	valid := map[Role]bool{}
	for _, r := range AllRoles {
		valid[r] = true
	}
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		switch {
		case p == "":
			return RoleSet{}, fmt.Errorf("--roles 含空项（取值是逗号分隔的 %s）", roleList())
		case !valid[Role(p)]:
			return RoleSet{}, fmt.Errorf("--roles 含未知角色 %q（可选 %s）", p, roleList())
		case rs.set[Role(p)]:
			return RoleSet{}, fmt.Errorf("--roles 重复指定了角色 %q", p)
		}
		rs.set[Role(p)] = true
	}
	return rs, nil
}

func roleList() string {
	ss := make([]string, len(AllRoles))
	for i, r := range AllRoles {
		ss[i] = string(r)
	}
	return strings.Join(ss, "|")
}

// Has 报告是否含角色 r。
func (rs RoleSet) Has(r Role) bool { return rs.set[r] }

// List 按固定顺序返回包含的角色。
func (rs RoleSet) List() []Role {
	var out []Role
	for _, r := range AllRoles {
		if rs.set[r] {
			out = append(out, r)
		}
	}
	return out
}

// String 返回规范形式（固定顺序，逗号分隔）。
func (rs RoleSet) String() string {
	var ss []string
	for _, r := range rs.List() {
		ss = append(ss, string(r))
	}
	return strings.Join(ss, ",")
}
