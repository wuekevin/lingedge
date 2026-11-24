package installer_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
)

// --set 语法：端口覆盖进 PortOverrides，其余进 Settings；坏输入被拒绝且不回显取值。
func TestParseSet(t *testing.T) {
	ok, err := installer.ParseSet([]string{"port.server.tcp.8080=18080@127.0.0.1", "port.node.udp.53=5353", "greeting=hello world", "port.server.tcp.9443=1443@[::1]"})
	if err != nil {
		t.Fatal(err)
	}
	want := []resolver.PortOverride{{Component: "node", Proto: "udp", Port: 53, To: 5353}, {Component: "server", Proto: "tcp", Port: 8080, To: 18080, Bind: "127.0.0.1"}, {Component: "server", Proto: "tcp", Port: 9443, To: 1443, Bind: "::1"}}
	if len(ok.Ports) != 3 || ok.Settings["greeting"] != "hello world" || len(ok.Settings) != 1 {
		t.Fatalf("%+v", ok)
	}
	for i := range want {
		if ok.Ports[i] != want[i] {
			t.Errorf("Ports[%d]=%+v 期望 %+v", i, ok.Ports[i], want[i])
		}
	}
	// 输入顺序无关。
	rev, _ := installer.ParseSet([]string{"port.server.tcp.9443=1443@[::1]", "greeting=hello world", "port.node.udp.53=5353", "port.server.tcp.8080=18080@127.0.0.1"})
	a, _ := json.Marshal(ok)
	b, _ := json.Marshal(rev)
	if string(a) != string(b) {
		t.Errorf("解析应与输入顺序无关")
	}
	for _, bad := range []string{
		"noequals", "Bad Key=1", "=x", "port.server.tcp.8080", "port.server.tcp.8080=0", "port.server.tcp.8080=65536", "port.server.tcp.8080=0x50",
		"port.server.tcp.8080=0080", "port.server.sctp.8080=1", "port.Server.tcp.8080=1", "port.server.tcp.0=1", "port.server.tcp.70000=1",
		"port.server.tcp.8080=1@notip", "port.server.tcp.8080=1@", "port.server=1", "port..tcp.1=2", "k=" + strings.Repeat("x", 1025), "k=a\x00b", "k=a\nb", "k=\x7f",
	} {
		_, err := installer.ParseSet([]string{bad})
		var se *installer.SetError
		if !errors.As(err, &se) {
			t.Errorf("应拒绝 %q: %v", bad, err)
			continue
		}
		if v := strings.SplitN(bad, "=", 2); len(v) == 2 && len(v[1]) > 3 && strings.Contains(se.Error(), v[1]) {
			t.Errorf("错误信息回显了值: %q", se.Error())
		}
	}
	if _, err := installer.ParseSet([]string{"a=1", "a=2"}); err == nil {
		t.Error("重复键应被拒绝")
	}
	many := make([]string, 65)
	for i := range many {
		many[i] = "k" + strings.Repeat("a", i) + "=1"
	}
	if _, err := installer.ParseSet(many); err == nil {
		t.Error("项数超限应被拒绝")
	}
}

// 外部可控入口的 fuzz：--set 解析不 panic，且接受的输出满足不变量（端口范围、键格式、重复键拒绝、顺序无关）。
func FuzzParseSet(f *testing.F) {
	for _, s := range []string{"port.server.tcp.8080=18080@127.0.0.1", "k=v", "port.a.udp.1=65535", "x=\x00", "port.x.tcp.1=2@[::1]", "=", "port.", "a.b-c_d=é"} {
		f.Add(s, "z=1")
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		out, err := installer.ParseSet([]string{a, b})
		if err != nil {
			return
		}
		for _, p := range out.Ports {
			if p.To < 1 || p.To > 65535 || p.Port < 1 || p.Port > 65535 || (p.Proto != "tcp" && p.Proto != "udp") || !registry.ValidComponent(p.Component) {
				t.Fatalf("接受了越界的端口覆盖: %+v", p)
			}
		}
		for k, v := range out.Settings {
			if strings.HasPrefix(k, "port.") || len(v) > 1024 || strings.ContainsAny(v, "\x00\n\r") {
				t.Fatalf("接受了非法设置 %q=%q", k, v)
			}
		}
		rev, err2 := installer.ParseSet([]string{b, a})
		if err2 != nil {
			t.Fatalf("顺序颠倒后应同样被接受: %v", err2)
		}
		x, _ := json.Marshal(out)
		y, _ := json.Marshal(rev)
		if string(x) != string(y) {
			t.Fatalf("解析与顺序有关: %s vs %s", x, y)
		}
	})
}

// tools.yaml：全部顶层节（release/install，以及 Story 1.42 起的 log/backup/doctor）严格校验（未知键、错误类型、非 https、相对路径一律拒绝）；缺省值。
func TestParseToolsConfig(t *testing.T) {
	cfg, err := installer.ParseToolsConfig([]byte("release:\n  base_url: https://releases.example.org/\n  ca_file: /etc/lingedge/ca.pem\n  proxy: socks5://127.0.0.1:1080\n  resume_dir: /var/lib/lingedge/tools/dl\ninstall:\n  health_timeout: 90s\n  health_interval: 2s\nlog:\n  level: debug # Story 1.42 起 log 节同样被严格校验\n"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := cfg.ManifestURL("beta")
	if err != nil || u != "https://releases.example.org/beta/manifest.json" {
		t.Fatalf("%q %v", u, err)
	}
	if d, _ := cfg.HealthTimeout(); d.Seconds() != 90 {
		t.Errorf("health_timeout=%v", d)
	}
	dc := cfg.DownloaderConfig("/state/downloads", true)
	if dc.CAFile != "/etc/lingedge/ca.pem" || dc.ProxyURL != "socks5://127.0.0.1:1080" || dc.ResumeDir != "/var/lib/lingedge/tools/dl" {
		t.Errorf("%+v", dc)
	}
	if dc := (installer.ToolsConfig{}).DownloaderConfig("/state/downloads", true); dc.ResumeDir != "/state/downloads" {
		t.Errorf("缺省续传目录: %+v", dc)
	}
	if dc := cfg.DownloaderConfig("/state/downloads", false); dc.ResumeDir != "" {
		t.Errorf("--dry-run 不得启用续传缓存: %+v", dc)
	}
	for _, bad := range []string{
		"release:\n  base_url: http://insecure.example\n", "release:\n  base_url: https://u" + ":" + "p@h/\n", "release:\n  unknown: 1\n", "release:\n  ca_file: relative.pem\n",
		"install:\n  health_timeout: forever\n", "install:\n  health_timeout: -1s\n", "install:\n  health_interval: 25h\n", "release: [1]\n", "- not a map\n", "release:\n  base_url: 5\n",
	} {
		if _, err := installer.ParseToolsConfig([]byte(bad)); err == nil || kindOf(t, err) != installer.KindConfig {
			t.Errorf("应拒绝 %q: %v", bad, err)
		}
	}
	if _, err := installer.ParseToolsConfig(nil); err != nil {
		t.Errorf("空配置 = 缺省: %v", err)
	}
	if _, err := (installer.ToolsConfig{}).ManifestURL("stable"); kindOf(t, err) != installer.KindPrecondition {
		t.Errorf("缺 base_url 应为前置条件失败: %v", err)
	}
}

// Story 1.42：log/backup/doctor 节也被严格校验；未知顶层节不再被忽略；Problems 返回全部取值问题（Validate 取第一个）。
func TestToolsConfigAllSections(t *testing.T) {
	good := "log:\n  level: warn\n  file: /var/log/lingedge/tools.log\nbackup:\n  dir: /var/lib/lingedge/backups\n  recent_max_age: 72h\n  custody_fingerprint: 0123456789abcdef\n" +
		"doctor:\n  min_memory_mb: 512\n  min_disk_mb: 1024\n  nofile_min: 8192\n  time_skew_warn: 2s\n  time_skew_fail: 1m\n  time_sync_servers: [ntp.example.org:123, '192.0.2.1:123']\n  restarts_warn: 5\n  offline: true\n"
	cfg, err := installer.ParseToolsConfig([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if w, _ := cfg.TimeSkewWarn(); w.Seconds() != 2 {
		t.Errorf("time_skew_warn=%v", w)
	}
	if f, _ := cfg.TimeSkewFail(); f.Minutes() != 1 {
		t.Errorf("time_skew_fail=%v", f)
	}
	if cfg.BackupRecentMaxAge().Hours() != 72 || (installer.ToolsConfig{}).BackupRecentMaxAge().Hours() != 168 {
		t.Errorf("backup.recent_max_age 取值/缺省：%v", cfg.BackupRecentMaxAge())
	}
	if !cfg.Doctor.Offline || cfg.Doctor.MinMemoryMB != 512 || len(cfg.Doctor.TimeSyncServers) != 2 {
		t.Errorf("%+v", cfg.Doctor)
	}
	for _, bad := range []string{
		"bogus:\n  a: 1\n", "log:\n  level: trace\n", "log:\n  file: rel.log\n", "log:\n  unknown: 1\n", "backup:\n  recent_max_age: 10m\n", "backup:\n  custody_fingerprint: ZZ\n",
		"backup:\n  dir: /a/../b\n", "doctor:\n  min_memory_mb: -1\n", "doctor:\n  restarts_warn: 99999999\n", "doctor:\n  time_skew_warn: 1h\n  time_skew_fail: 1m\n",
		"doctor:\n  time_sync_servers: [nohostport]\n", "doctor:\n  time_sync_servers: ['a:1','b:1','c:1','d:1','e:1','f:1','g:1','h:1','i:1']\n", "doctor:\n  offline: maybe\n",
		"release:\n  proxy: ftp://x\n",
	} {
		if _, err := installer.ParseToolsConfig([]byte(bad)); err == nil || kindOf(t, err) != installer.KindConfig {
			t.Errorf("应拒绝 %q: %v", bad, err)
		}
	}
	// Problems 返回全部问题，路径稳定；Validate 与第一个一致。
	multi := installer.ToolsConfig{Log: installer.LogConfig{Level: "x"}, Doctor: installer.DoctorConfig{MinMemoryMB: -1}}
	ps := multi.Problems()
	if len(ps) != 2 || ps[0].Path != "log.level" || ps[1].Path != "doctor.min_memory_mb" {
		t.Errorf("Problems=%+v", ps)
	}
	if err := multi.Validate(); err == nil || !strings.Contains(err.Error(), "log.level") {
		t.Errorf("Validate 应返回第一个问题：%v", err)
	}
	if installer.CheckHostPort("ntp.example.org:123") != nil || installer.CheckHostPort("[2001:db8::1]:123") != nil {
		t.Error("合法 host:port 被拒绝")
	}
	for _, bad := range []string{"", ":123", "host", "host:0", "host:70000", "bad host:1", "-x.example:1"} {
		if installer.CheckHostPort(bad) == nil {
			t.Errorf("应拒绝 %q", bad)
		}
	}
}

func FuzzParseToolsConfig(f *testing.F) {
	f.Add([]byte("release:\n  base_url: https://x/\n"))
	f.Add([]byte("install:\n  health_timeout: 1s\n"))
	f.Add([]byte("{{{"))
	f.Fuzz(func(t *testing.T, b []byte) {
		cfg, err := installer.ParseToolsConfig(b)
		if err != nil {
			return
		}
		if cfg.Release.BaseURL != "" && !strings.HasPrefix(cfg.Release.BaseURL, "https://") {
			t.Fatalf("接受了非 https 的 base_url: %q", cfg.Release.BaseURL)
		}
	})
}

// journal 里的请求（recover --resume 的输入）：第 1 步的 undo 携带它，解码后与原请求一致（含已求解的 Pinned、设置与端口覆盖）。
func TestRequestJournalRoundTrip(t *testing.T) {
	w := newW(t)
	o := doInstall(t, w, "node", []func(*installer.Deps){failAt(nil, []int{21})}, func(r *installer.Request) {
		r.Settings = map[string]string{"greeting": "hi"}
		r.PortOverrides = nil
	}) // 提交点之后 journal 故障：journal 留存，便于读取
	must(t, o)
	js := activeJournals(t, w)
	if len(js) != 1 {
		t.Fatal("应留有 journal")
	}
	tx, err := w.Registry("x").ReadTx(strings.TrimSuffix(filepath.Base(js[0]), ".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var got *installer.Request
	for _, s := range tx.Steps {
		if s.Step == 1 && s.Undo != nil {
			for _, a := range s.Undo.Actions {
				if r, isReq, err := installer.DecodeRequest(a); isReq && err == nil {
					got = &r
				}
			}
		}
	}
	if got == nil || got.Component != "node" || got.Channel != "stable" || got.Offline != w.Bundle || got.Settings["greeting"] != "hi" ||
		len(got.Pinned) != 1 || got.Pinned[0].Component != "node" || got.Pinned[0].Version != "0.1.0" {
		t.Fatalf("journal 中的请求: %+v", got)
	}
	if got.Yes || got.DryRun || got.NonInteractive {
		t.Errorf("交互设置不应写入 journal: %+v", got)
	}
}

func FuzzDecodeRequest(f *testing.F) {
	f.Add(`{"component":"node","channel":"stable"}`, "1")
	f.Add(`{`, "1")
	f.Add(`{"component":"node"}`, "2")
	f.Fuzz(func(t *testing.T, body, v string) {
		a := registry.UndoAction{Action: "request", Extra: map[string]string{"v": v, "request": body}}
		r, isReq, err := installer.DecodeRequest(a)
		if !isReq {
			t.Fatal("request 动作必须被识别")
		}
		if err == nil && v != "1" {
			t.Fatalf("未知版本不应被接受: %+v", r)
		}
	})
}
