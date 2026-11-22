package setupit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type proc struct {
	t      *testing.T
	cmd    *exec.Cmd
	done   chan error
	url    string
	exited atomic.Bool
}

// startServer 启动真实 lingedge-server 二进制（与生产构建同一入口），stderr 并入 w.logs（泄漏扫描），等待 /healthz 存活。
func (w *wiz) startServer(t *testing.T, cfgPath, addr string) *proc {
	t.Helper()
	cmd := exec.Command(serverBin(t), "--config", cfgPath)
	cmd.Env = cleanEnv()
	cmd.Stdout, cmd.Stderr = w.logs, w.logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &proc{t: t, cmd: cmd, done: make(chan error, 1), url: "http://" + addr}
	go func() {
		err := cmd.Wait()
		p.exited.Store(true)
		p.done <- err
	}()
	t.Cleanup(func() { p.kill() })
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-p.done:
			t.Fatalf("服务进程提前退出: %v\n%s", err, w.logs.String())
		default:
		}
		if res, err := http.Get(p.url + "/healthz"); err == nil {
			_ = res.Body.Close()
			if res.StatusCode == 200 {
				return p
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("服务进程未在时限内就绪\n%s", w.logs.String())
	return nil
}

func (p *proc) kill() {
	if p.exited.Load() {
		return
	}
	_ = p.cmd.Process.Kill()
	select {
	case err := <-p.done:
		_ = err // 被 SIGKILL 终止的退出错误是预期的
	case <-time.After(10 * time.Second):
	}
}

// term 发送 SIGTERM 并等待退出码（优雅停机应为 0）。
func (p *proc) term() int {
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-p.done:
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return 0
	case <-time.After(30 * time.Second):
		p.t.Fatal("进程未在时限内退出")
		return -1
	}
}

func (w *wiz) processConfig(t *testing.T) (cfgPath, httpAddr string) {
	t.Helper()
	httpAddr = freeAddr(t)
	y := fmt.Sprintf(`server:
  env: test
  data_dir: %s
http:
  listen: %s
grpc:
  listen: %s
  enroll_listen: %s
metrics:
  listen: %s
log:
  level: info
  format: json
secrets:
  master_key_file: %s
  setup_token_hash_file: %s
auth:
  argon2:
    memory_kib: 19456
    iterations: 2
    parallelism: 1
`, w.dataDir, httpAddr, freeAddr(t), freeAddr(t), freeAddr(t), w.keyFile, w.hashFile)
	cfgPath = filepath.Join(w.dir, "proc-server.yaml")
	if err := os.WriteFile(cfgPath, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return
}

// tryStatus 与 httpStatus 相同，但连接错误（切换瞬间的拒绝/重置）返回状态 0 而不是失败。
func tryStatus(t *testing.T, method, url string, body any, hdr ...string) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil
	}
	defer func() { _ = res.Body.Close() }()
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	return res.StatusCode, m
}

func httpStatus(t *testing.T, method, url string, body any, hdr ...string) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	return res.StatusCode, m
}

// 真实进程端到端（AC1/AC4/AC5）：无 install.lock 启动 → setup mode（其余 API 503）→ 向导 → complete 后**同一进程内**切换为正常模式（登录刚创建的管理员、
// 向导路由 410、setup mode 的 /setup 页消失）→ 优雅停机退出码 0；重启直接进入正常模式；删除 install.lock 后据数据库记录自愈补写并进入正常模式；
// 向导中途被 SIGKILL 后重启仍在 setup mode 且可续做；全程日志无任何机密。
func TestRealProcessSetupModeLifecycle(t *testing.T) {
	w := newWiz(t)
	w.stop() // 进程内向导服务不用；本用例只用 wiz 的环境（临时 MySQL/Redis/目录/口令/扫描）
	cfgPath, addr := w.processConfig(t)

	// ---- 阶段一：setup mode ----
	p := w.startServer(t, cfgPath, addr)
	w.baseURL = p.url
	if code, m := httpStatus(t, "POST", p.url+"/api/v1/auth/login", map[string]string{"realm": "admin", "email": w.admEmail, "password": w.admPW}); code != 503 {
		t.Fatalf("setup mode 下认证 API 应 503: %d %v", code, m)
	}
	if code, _ := httpStatus(t, "GET", p.url+"/setup", nil); code != 200 {
		t.Errorf("/setup 应可用: %d", code)
	}
	if code, _ := httpStatus(t, "GET", p.url+"/healthz/ready", nil); code != 503 {
		t.Errorf("/healthz/ready 应 503: %d", code)
	}
	if r := w.do("GET", api+"status", "", nil); r.Status != 401 {
		t.Errorf("缺 token 应 401: %d", r.Status)
	}
	w.mustOK(w.post("checks", nil), "checks")
	w.mustOK(w.put("database", w.dbBody()), "database")
	w.mustOK(w.put("redis", w.redisBody()), "redis")
	// 向导中途 SIGKILL（进程内存全部丢失）：重启后仍是 setup mode，进度由持久状态重建，可续做。
	p.kill()
	p = w.startServer(t, cfgPath, addr)
	if code, _ := httpStatus(t, "POST", p.url+"/api/v1/auth/login", map[string]string{"realm": "admin", "email": w.admEmail, "password": w.admPW}); code != 503 {
		t.Fatalf("重启后（无 install.lock）仍应是 setup mode: %d", code)
	}
	w.wantStatuses(t, "SIGKILL 重启后", "database", "redis")
	w.mustOK(w.put("database", w.dbBody()), "database(续做，重新提供迁移口令)")
	w.mustOK(w.post("initialize", nil), "initialize")
	w.mustOK(w.post("admin", w.adminBody()), "admin")
	w.mustOK(w.put("system", w.systemBody()), "system")
	w.mustOK(w.post("keys", nil), "keys")
	w.mustOK(w.post("complete", nil), "complete")

	// ---- 同一进程内切换为正常模式 ----
	deadline := time.Now().Add(30 * time.Second)
	var code int
	for time.Now().Before(deadline) {
		code, _ = tryStatus(t, "POST", p.url+"/api/v1/auth/login", map[string]string{"realm": "admin", "email": w.admEmail, "password": w.admPW}, "X-Forwarded-For", "203.0.113.5")
		if code != 503 && code != 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code != 200 {
		t.Fatalf("complete 后同一进程应切换为正常模式并允许登录: %d\n%s", code, w.logs.String())
	}
	if code, _ := httpStatus(t, "GET", p.url+"/api/v1/public/setup/status", nil, "X-Setup-Token", w.token); code != 410 {
		t.Errorf("正常模式下向导路由应 410: %d", code)
	}
	if code, m := httpStatus(t, "GET", p.url+"/setup", nil); code != 410 {
		t.Errorf("正常模式下 /setup 应 410（setup.already_installed，Story 1.37 / MIN-04）: %d", code)
	} else if e, _ := m["error"].(map[string]any); e == nil || e["code"] != "setup.already_installed" {
		t.Errorf("/setup 的 410 应带稳定错误码 setup.already_installed: %v", m)
	}
	if code, _ := httpStatus(t, "GET", p.url+"/healthz/ready", nil); code != 200 {
		t.Errorf("正常模式 /healthz/ready 应 200: %d", code)
	}
	if rc := p.term(); rc != 0 {
		t.Errorf("正常模式优雅停机退出码应为 0: %d\n%s", rc, w.logs.String())
	}

	// ---- 重启：有 install.lock → 直接正常模式 ----
	p = w.startServer(t, cfgPath, addr)
	if code, _ := httpStatus(t, "POST", p.url+"/api/v1/auth/login", map[string]string{"realm": "admin", "email": w.admEmail, "password": w.admPW}, "X-Forwarded-For", "203.0.113.6"); code != 200 {
		t.Fatalf("重启后应直接进入正常模式: %d", code)
	}
	if rc := p.term(); rc != 0 {
		t.Errorf("退出码 %d", rc)
	}

	// ---- 删除 install.lock：据数据库里完成的安装记录自愈补写，仍进入正常模式（不会回到 setup mode 重置系统）----
	if err := os.Remove(w.lockFile); err != nil {
		t.Fatal(err)
	}
	p = w.startServer(t, cfgPath, addr)
	if code, _ := httpStatus(t, "POST", p.url+"/api/v1/auth/login", map[string]string{"realm": "admin", "email": w.admEmail, "password": w.admPW}, "X-Forwarded-For", "203.0.113.7"); code != 200 {
		t.Fatalf("install.lock 缺失但数据库记录完成：应自愈并进入正常模式: %d\n%s", code, w.logs.String())
	}
	if _, err := os.Stat(w.lockFile); err != nil {
		t.Errorf("install.lock 应已被补写: %v", err)
	}
	if rc := p.term(); rc != 0 {
		t.Errorf("退出码 %d", rc)
	}
	if !strings.Contains(w.logs.String(), "setup mode") {
		t.Error("日志应记录进入 setup mode")
	}
	w.assertNoSecrets()
}

// 没有任何数据库配置、没有 install.lock、主密钥缺失：Server 仍进入 setup mode（环境检查会报告主密钥问题），而不是启动失败——否则操作者看不到任何提示。
func TestRealProcessSetupModeWithoutMasterKey(t *testing.T) {
	w := newWiz(t)
	w.stop()
	cfgPath, addr := w.processConfig(t)
	if err := os.Remove(w.keyFile); err != nil {
		t.Fatal(err)
	}
	p := w.startServer(t, cfgPath, addr)
	w.baseURL = p.url
	r := w.post("checks", nil)
	bad := false
	for _, it := range r.JSON["items"].([]any) {
		if m := it.(map[string]any); m["key"] == "master_key.readable" && m["ok"] == false {
			bad = true
		}
	}
	if !bad {
		t.Errorf("环境检查应报告主密钥不可读: %s", r.Raw)
	}
	if r := w.put("database", w.dbBody()); r.Status != 400 || r.errCode() != "setup.precheck_failed" {
		t.Errorf("无主密钥时 MySQL 步骤应拒绝: %d %s", r.Status, r.Raw)
	}
	if code, _ := httpStatus(t, "GET", p.url+"/api/v1/sites", nil); code != 503 {
		t.Errorf("其余 API 503: %d", code)
	}
	if rc := p.term(); rc != 0 {
		t.Errorf("setup mode 收到 SIGTERM 应退出码 0: %d", rc)
	}
}
