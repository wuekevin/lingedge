package dbmigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// 迁移分类（与 lingedge-server migrate plan 的 class 取值一致）。
const (
	ClassReversible   = "reversible"
	ClassExpand       = "expand"
	ClassIrreversible = "irreversible"
)

// Item 是 plan 中的一项迁移。
type Item struct {
	Version int64  `json:"version"`
	Name    string `json:"name"`
	Class   string `json:"class"`
	HasDown bool   `json:"has_down"`
	Summary string `json:"summary,omitempty"`
}

// Plan 是 `migrate plan` 的 JSON。连库时 Migrations 是待执行迁移、DBVersion 是库已应用的最大序号；离线时列出全部内嵌迁移。
type Plan struct {
	Mode            string `json:"mode"`
	DBVersion       *int64 `json:"db_version"`
	LatestKnown     int64  `json:"latest_version"`
	Count           int    `json:"count"`
	Migrations      []Item `json:"migrations"`
	HasIrreversible bool   `json:"has_irreversible"`
	MaxClass        string `json:"max_class"`
}

// Classes 返回计划里出现的迁移分类（固定顺序 reversible/expand/irreversible，去重）。
func (p *Plan) Classes() []string {
	seen := map[string]bool{}
	for _, m := range p.Migrations {
		seen[m.Class] = true
	}
	var out []string
	for _, c := range []string{ClassReversible, ClassExpand, ClassIrreversible} {
		if seen[c] {
			out = append(out, c)
		}
	}
	return out
}

// Status 是 `migrate status --json` 的子集。
type Status struct {
	DBVersion   int64  `json:"db_version"`
	LatestKnown int64  `json:"latest_version"`
	Pending     int    `json:"pending"`
	Problem     string `json:"problem,omitempty"`
}

// Applied 是 up/down 的结果。
type Applied struct {
	FromVersion int64 `json:"from_version"`
	ToVersion   int64 `json:"to_version"`
}

// Error 是 migrate 子进程失败。ExitCode 沿用 lingedge-server migrate 的退出码（4 dirty、5 校验和、9 含不可逆但未确认、10 迁移集合不一致…；-1 = 未能执行）。
// Message 已经 safeexec 脱敏。
type Error struct {
	Op       string
	ExitCode int
	Message  string
}

func (e *Error) Error() string {
	return fmt.Sprintf("lingedge-server migrate %s 失败（退出码 %d）：%s", e.Op, e.ExitCode, e.Message)
}

// Migrator 调用某个 lingedge-server 二进制的 migrate 子命令。
type Migrator struct {
	// Run 是只允许 lingedge-server 的 Runner（backup.NewServerRunner）。
	Run service.Runner
	// Env 是额外环境变量（不得含口令；测试世界用）。
	Env []string
}

// EnvUser/EnvPassword 与 lingedge-server migrate 同名（backup 包常量的别名）。
const (
	EnvUser     = backup.EnvUser
	EnvPassword = backup.EnvDBPass
)

func (m *Migrator) args(sub string, conn *backup.DBConn, extra ...string) ([]string, []string, error) {
	if conn == nil {
		return nil, nil, errors.New("缺少数据库连接")
	}
	a := []string{"migrate", sub}
	switch {
	case conn.Socket != "":
		a = append(a, "--socket", conn.Socket)
	case conn.Host != "":
		a = append(a, "--host", conn.Host)
		if conn.Port != 0 {
			a = append(a, "--port", strconv.Itoa(conn.Port))
		}
	default:
		return nil, nil, backup.ErrNoTarget
	}
	a = append(a, "--database", conn.Name, "--user", conn.User)
	if conn.TLS != "" {
		a = append(a, "--tls", conn.TLS)
	}
	a = append(a, extra...)
	env := append([]string(nil), m.Env...)
	env = append(env, EnvPassword+"="+string(conn.Password))
	return a, env, nil
}

func (m *Migrator) exec(ctx context.Context, bin, op string, args, env []string) ([]byte, error) {
	if m.Run == nil {
		return nil, errors.New("dbmigrate: 未配置 lingedge-server 执行器")
	}
	if !filepath.IsAbs(bin) {
		return nil, fmt.Errorf("dbmigrate: 二进制路径必须是绝对路径: %q", bin)
	}
	res, err := m.Run.Run(ctx, safeexec.Cmd{Path: bin, Args: args, Env: env})
	if err != nil {
		var ee *safeexec.ExitError
		if errors.As(err, &ee) {
			return res.Stdout, &Error{Op: op, ExitCode: ee.Code, Message: ee.Stderr}
		}
		return nil, &Error{Op: op, ExitCode: -1, Message: err.Error()}
	}
	return res.Stdout, nil
}

// PlanOffline 列出二进制内嵌的全部迁移（不连库，无需凭据）。
func (m *Migrator) PlanOffline(ctx context.Context, bin string) (*Plan, error) {
	out, err := m.exec(ctx, bin, "plan", []string{"migrate", "plan", "--offline"}, m.Env)
	if err != nil {
		return nil, err
	}
	return parsePlan(out)
}

// Plan 连库列出待执行迁移（步骤 7；只读）。
func (m *Migrator) Plan(ctx context.Context, bin string, conn *backup.DBConn) (*Plan, error) {
	a, env, err := m.args("plan", conn)
	if err != nil {
		return nil, err
	}
	out, err := m.exec(ctx, bin, "plan", a, env)
	if err != nil {
		return nil, err
	}
	return parsePlan(out)
}

func parsePlan(b []byte) (*Plan, error) {
	var p Plan
	if err := json.Unmarshal(b, &p); err != nil || p.LatestKnown < 1 {
		return nil, errors.New("无法解析 lingedge-server migrate plan 的输出")
	}
	if p.Migrations == nil {
		p.Migrations = []Item{}
	}
	return &p, nil
}

// Status 返回库的迁移状态（只读）。
func (m *Migrator) Status(ctx context.Context, bin string, conn *backup.DBConn) (*Status, error) {
	a, env, err := m.args("status", conn, "--json")
	if err != nil {
		return nil, err
	}
	out, err := m.exec(ctx, bin, "status", a, env)
	if err != nil {
		return nil, err
	}
	var s Status
	if err := json.Unmarshal(out, &s); err != nil {
		return nil, errors.New("无法解析 lingedge-server migrate status 的输出")
	}
	return &s, nil
}

// Up 应用待执行迁移（步骤 9）。acceptIrreversible 对应 --accept-irreversible：只有 Tools 已经取得用户确认（交互键入 confirm-irreversible 或非交互 --accept-irreversible）才传。
func (m *Migrator) Up(ctx context.Context, bin string, conn *backup.DBConn, acceptIrreversible bool) (*Applied, error) {
	extra := []string{"--json"}
	if acceptIrreversible {
		extra = append(extra, "--accept-irreversible")
	}
	a, env, err := m.args("up", conn, extra...)
	if err != nil {
		return nil, err
	}
	out, err := m.exec(ctx, bin, "up", a, env)
	if err != nil {
		return nil, err
	}
	return parseApplied(out)
}

// Down 回退到 to（该序号保留；仅 reversible 迁移可回退）。必须用**含这些迁移的那份**（新）二进制执行。
func (m *Migrator) Down(ctx context.Context, bin string, conn *backup.DBConn, to int64) (*Applied, error) {
	if to < 1 {
		return nil, errors.New("dbmigrate: down 的目标版本必须 ≥ 1")
	}
	a, env, err := m.args("down", conn, "--to", strconv.FormatInt(to, 10), "--json")
	if err != nil {
		return nil, err
	}
	out, err := m.exec(ctx, bin, "down", a, env)
	if err != nil {
		return nil, err
	}
	return parseApplied(out)
}

func parseApplied(b []byte) (*Applied, error) {
	var a Applied
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, errors.New("无法解析 lingedge-server migrate 的 JSON 输出")
	}
	return &a, nil
}
