package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/security"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// 撤销动作名（registry.UndoAction.Action）。执行者是 installer.Undoer（recover --rollback）与 Service 的进程内回滚——两条路径共用同一份实现。
const (
	// ActRestoreRequest：restore 事务的请求记录（撤销为空操作）。
	ActRestoreRequest = "restore_request"
	// ActRestoreSafety：恢复到「恢复前安全备份」的状态（文件 + 数据库）并按需重启服务。
	ActRestoreSafety = "restore_safety_backup"
	// ActRestoreMasterKey：把 master.key 恢复为恢复前的样子（换回被保存的旧文件，或删除本次新装的）。
	ActRestoreMasterKey = "restore_master_key"
	// ExtraKeys（UndoAction.Extra 的键）。
	ExtraComponent = "component"
	ExtraUnits     = "units"
	ExtraWasActive = "was_active"
	ExtraEncrypted = "encrypted"
	ExtraSaved     = "saved"
	ExtraCreated   = "created"
)

// ---- 文件写入 ----

type destOwner struct{ owner, group string }

// writeFromStage 把暂存文件原子地写到 dst：同目录临时文件 → 设权限 → chown → fsync → rename。不跟随符号链接，拒绝覆盖目录/符号链接。
func (s *Service) writeFromStage(dst, src string, mode fs.FileMode, ow destOwner) error {
	if err := s.ensureParents(filepath.Dir(dst), ow); err != nil {
		return err
	}
	if fi, err := os.Lstat(dst); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("目标 %s 已存在且不是普通文件（符号链接/目录），拒绝覆盖", s.virtual(dst))
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".restore-"+randHex(4))
	out, err := security.CreateFile(tmp, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = out.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Chmod(mode); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := s.chown(tmp, ow); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(filepath.Dir(dst)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (s *Service) chown(path string, ow destOwner) error {
	if ow.owner == "" || s.Own == nil {
		return nil
	}
	uid, gid, err := s.Own.Resolve(ow.owner, ow.group)
	if err != nil {
		return fmt.Errorf("解析属主 %s:%s 失败: %w", ow.owner, ow.group, err)
	}
	return s.Own.Chown(path, uid, gid)
}

// ensureParents 逐级确认 dir 在（虚拟）根之内且沿途没有符号链接；缺失的目录以 0750 创建并设为文件的属主。
func (s *Service) ensureParents(dir string, ow destOwner) error {
	base := s.Root
	if base == "" {
		base = "/"
	}
	rel, err := filepath.Rel(base, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("目标目录 %s 不在根目录之内", dir)
	}
	cur := base
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(cur, 0o750); err != nil {
				return err
			}
			if err := s.chown(cur, ow); err != nil {
				return err
			}
		case err != nil:
			return err
		case fi.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("目标路径 %s 经过符号链接 %s，拒绝", s.virtual(dir), s.virtual(cur))
		case !fi.IsDir():
			return fmt.Errorf("目标路径 %s 的上级 %s 不是目录", s.virtual(dir), s.virtual(cur))
		}
	}
	return nil
}

// UndoMasterKey 执行 ActRestoreMasterKey（幂等）：存在被保存的旧文件 → 换回；否则若本次是新装的 → 删除。
func UndoMasterKey(a registry.UndoAction) (service.UndoResult, error) {
	if a.Path == "" || filepath.Base(a.Path) != "master.key" {
		return service.UndoResult{}, errors.New("撤销路径不是 master.key，拒绝")
	}
	saved := a.Extra[ExtraSaved]
	if saved != "" {
		if filepath.Dir(saved) != filepath.Dir(a.Path) || !strings.HasPrefix(filepath.Base(saved), "master.key.pre-restore-") {
			return service.UndoResult{}, errors.New("被保存的主密钥路径不合法，拒绝")
		}
		if fi, err := os.Lstat(saved); err == nil && fi.Mode().IsRegular() {
			if err := os.Rename(saved, a.Path); err != nil {
				return service.UndoResult{}, err
			}
			return service.UndoResult{Outcome: service.OutcomeDone, Detail: "已换回恢复前的 master.key"}, nil
		}
	}
	if a.Extra[ExtraCreated] == "true" {
		if err := os.Remove(a.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return service.UndoResult{}, err
		}
		return service.UndoResult{Outcome: service.OutcomeDone, Detail: "已删除本次安装的 master.key"}, nil
	}
	return service.UndoResult{Outcome: service.OutcomeNoop}, nil
}

// ---- 二进制与 lineage ----

func (s *Service) serverBin() string { return s.host(ServerCurrentBin) }

func (s *Service) serverRunner() (service.Runner, error) {
	if s.ServerRun == nil {
		return nil, errors.New("backup: 未配置 lingedge-server 执行器")
	}
	return s.ServerRun, nil
}

// binarySchema 返回当前 Server 二进制内嵌的最新迁移版本（`lingedge-server migrate plan --offline`，不连库）。
func (s *Service) binarySchema(ctx context.Context) (int64, error) {
	run, err := s.serverRunner()
	if err != nil {
		return 0, err
	}
	res, err := run.Run(ctx, safeexec.Cmd{Path: s.serverBin(), Args: []string{"migrate", "plan", "--offline"}, Env: s.ServerEnv})
	if err != nil {
		return 0, fmt.Errorf("无法确定当前 Server 二进制的迁移级别（lingedge-server migrate plan --offline）: %w", err)
	}
	var p struct {
		Latest int64 `json:"latest_version"`
	}
	if err := json.Unmarshal(res.Stdout, &p); err != nil || p.Latest < 1 {
		return 0, errors.New("无法解析 lingedge-server migrate plan 的输出")
	}
	return p.Latest, nil
}

// bumpLineage 在数据库恢复之后、启动 Server 之前执行 `lingedge-server lineage bump`（IMP-07/H6，tools-design §17.5）。
func (s *Service) bumpLineage(ctx context.Context) (before, after int64, err error) {
	run, err := s.serverRunner()
	if err != nil {
		return 0, 0, err
	}
	cfg := s.ServerConfig
	if cfg == "" {
		cfg = s.host(ServerConfigFile)
	}
	env := append(append([]string(nil), s.ServerEnv...), "LINGEDGE_SECRETS__MASTER_KEY_FILE="+s.host(ServerMasterKey))
	res, err := run.Run(ctx, safeexec.Cmd{Path: s.serverBin(), Args: []string{"lineage", "bump", "--config", cfg, "--json"}, Env: env})
	if err != nil {
		return 0, 0, fmt.Errorf("lingedge-server lineage bump 失败: %w", err)
	}
	var out struct {
		Before int64 `json:"before"`
		After  int64 `json:"after"`
	}
	if err := json.Unmarshal(res.Stdout, &out); err != nil || out.After < 1 {
		return 0, 0, errors.New("无法解析 lineage bump 的输出")
	}
	if out.After <= out.Before {
		return 0, 0, fmt.Errorf("lineage bump 没有严格提升（%d -> %d）", out.Before, out.After)
	}
	return out.Before, out.After, nil
}

// ---- 服务控制与健康检查 ----

func (s *Service) unitsFor(component string) []service.UnitName {
	return service.EnableTargets(component)
}

func isActiveState(st string) bool { return st == "active" || st == "activating" }

func (s *Service) activeUnits(ctx context.Context, units []service.UnitName) (bool, error) {
	if s.Sys == nil {
		return false, errors.New("backup: 未配置 systemd 控制")
	}
	for _, u := range units {
		st, err := s.Sys.IsActive(ctx, u)
		if err != nil {
			return false, err
		}
		if isActiveState(st) {
			return true, nil
		}
	}
	return false, nil
}

func parseUnits(csv string) ([]service.UnitName, error) {
	var out []service.UnitName
	for _, p := range strings.Split(csv, ",") {
		if p == "" {
			continue
		}
		u := service.UnitName(p)
		if !u.Valid() {
			return nil, fmt.Errorf("未知单元 %q", p)
		}
		out = append(out, u)
	}
	return out, nil
}

func unitsCSV(us []service.UnitName) string {
	var p []string
	for _, u := range us {
		p = append(p, string(u))
	}
	return strings.Join(p, ",")
}

// serverHealthURL 由（恢复后的）server.yaml 的 http.listen 得到本机 /healthz 的 URL；通配主机映射为回环。
func (s *Service) serverHealthURL(path string) (string, error) {
	cfg := s.host(ServerConfigFile)
	b, err := os.ReadFile(cfg)
	if err != nil {
		return "", err
	}
	var y struct {
		HTTP struct {
			Listen string `yaml:"listen"`
		} `yaml:"http"`
	}
	if err := yaml.Unmarshal(b, &y); err != nil {
		return "", errors.New("server.yaml 解析失败")
	}
	host, port, err := net.SplitHostPort(y.HTTP.Listen)
	if err != nil || port == "" {
		host, port = "127.0.0.1", "8080"
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if a, perr := netip.ParseAddr(host); host == "" || (perr == nil && a.IsUnspecified()) {
		host = "127.0.0.1"
		if perr == nil && a.Is6() {
			host = "::1"
		}
	}
	return "http://" + net.JoinHostPort(host, port) + path, nil
}

func httpOK(ctx context.Context, url string) error {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	c := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GET %s 返回 %d", url, resp.StatusCode)
	}
	return nil
}

// waitHealthy 在窗口内重复探测直到成功或超时。
func (s *Service) waitHealthy(ctx context.Context, url string) error {
	check := s.Health
	if check == nil {
		check = httpOK
	}
	timeout, every := s.HealthTimeout, s.HealthInterval
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	if every <= 0 {
		every = time.Second
	}
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if last = check(ctx, url); last == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("健康检查在 %s 内未通过: %w", timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}
