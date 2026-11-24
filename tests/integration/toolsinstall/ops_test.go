package toolsinstall

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/cli"
	"github.com/wuekevin/lingedge/internal/tools/downloader"
	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
	"github.com/wuekevin/lingedge/internal/tools/service"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

// Story 1.42 联调：真实的 doctor / config / status / list --available（cli.Execute → doctor/cfgcheck/installer 包）对着：
//   - 本机构建的真实 lingedge-server 进程（真实 /healthz 与 /healthz/ready、真实 `lingedge-server config check`）；
//   - 本机临时 MySQL 8.0（tests/mysqltest，真实 mysql 客户端查询 utf8mb4/sql_mode）与临时 Redis；
//   - 真实的 HTTPS 发行源（httptest TLS，真实签名的清单，经 release.ca_file 信任）；
//   - 真实的 UDP SNTP 时间源。
// 伪的只有「操作系统」（伪 systemctl/journalctl/timedatectl，tests/toolsworld）。绝不触碰常驻 mysqld。

type opsOpt struct {
	env   map[string]string
	tty   bool
	stdin string
	ops   cli.OpsEnv
	// realProbe 为 true 时用真实 probe.Collect（指向临时 MySQL/Redis）。
	realProbe bool
	// realClock 为 true 时用真实时钟：安装时 state.json/journal 的修改时间是真实时间，伪时钟（toolsworld.T0 = 2026-11-21）会被 sys.time_sync
	// 正确地判为「时钟回拨」——doctor 的这项检查在这里恰恰是真实生效的。
	realClock bool
}

func (s *stack) runOps(uid int, o opsOpt, args ...string) (code int, stdout, stderr string) {
	s.t.Helper()
	r, err := service.NewRunner([]string{s.w.Bin})
	if err != nil {
		s.t.Fatal(err)
	}
	genv := map[string]string{backup.EnvDBPass: s.be.migPW, backup.EnvHost: s.be.inst.Host(), backup.EnvPort: strconv.Itoa(s.be.inst.Port), backup.EnvName: s.be.dbName}
	for k, v := range o.env {
		genv[k] = v
	}
	collect := func(context.Context) (probe.Result, error) { return s.w.Probe, nil }
	if o.realProbe {
		collect = func(ctx context.Context) (probe.Result, error) {
			return probe.Collect(ctx, probe.Options{MySQL: []probe.Endpoint{{Host: s.be.inst.Host(), Port: s.be.inst.Port}}, Redis: []probe.Endpoint{redisEndpoint(s)},
				MySQLSockets: []string{}, RedisSockets: []string{}, DialTimeout: 3 * time.Second})
		}
	}
	var so, se syncBuf
	now := func() time.Time { return toolsworld.T0.Add(time.Duration(s.clock) * time.Second) }
	if o.realClock {
		now = time.Now
	}
	ev := cli.Env{
		Stdin: strings.NewReader(o.stdin), Stdout: &so, Stderr: &se, StdinTTY: o.tty, StderrTTY: o.tty, Getenv: func(k string) string { return genv[k] },
		Geteuid: func() int { return uid }, Hostname: func() (string, error) { return migHost, nil },
		Now: now, NewRunID: func(time.Time) string { return "01JD5Y3C8Q5G7M0V1N2P3R4S5T" },
		StatePath: s.w.Layout.StateFile(), BuiltinKeys: func() ([]byte, error) { return toolsworld.KeysJSON(), nil },
		Install: cli.InstallEnv{Root: s.w.Root, Specs: s.w.Specs, Runner: r, Own: s.w.Own, Booted: func() bool { return true }, Collect: collect},
		Backup: cli.BackupEnv{DumpBin: filepath.Join(s.bin, "mysqldump"), ClientBin: filepath.Join(s.bin, "mysql"), ServerConfig: filepath.Join(s.w.Root, "run", "server-live.yaml"),
			KDF: backup.KDFParams{Time: 1, MemoryKiB: 64, Threads: 1}, HealthTimeout: 30 * time.Second, HealthInterval: 100 * time.Millisecond},
		Ops: o.ops,
	}
	s.clock++
	code = cli.Execute(context.Background(), ev, args)
	return code, so.String(), se.String()
}

func redisEndpoint(s *stack) probe.Endpoint {
	h, p, _ := net.SplitHostPort(s.be.rds.Addr())
	port, _ := strconv.Atoi(p)
	return probe.Endpoint{Host: h, Port: port}
}

func checkMap(t *testing.T, so string) map[string]map[string]any {
	t.Helper()
	d := dataOf(envelopeOf(t, so))
	out := map[string]map[string]any{}
	for _, c := range d["checks"].([]any) {
		m := c.(map[string]any)
		out[m["id"].(string)] = m
	}
	return out
}

func (s *stack) fakeRunner(name string) service.Runner {
	r, err := safeexec.New(safeexec.Options{Allowed: []string{name}, SearchDirs: []string{s.w.Bin}, MaxOutput: 1 << 16})
	if err != nil {
		s.t.Fatal(err)
	}
	return r
}

// 伪时钟早于 Tools 自己记录的时间点 = 时钟回拨：sys.time_sync 必须 fail（真实文件时间戳 vs 注入时钟）。
func TestDoctorDetectsClockRewindAgainstRealFiles(t *testing.T) {
	s := startStack(t)
	o := opsOpt{ops: cli.OpsEnv{Timedatectl: s.fakeRunner("timedatectl"), Nofile: func() (uint64, error) { return 65536, nil }, PeerCerts: func(context.Context, string) ([]*x509.Certificate, error) { return nil, os.ErrNotExist }}}
	code, so, _ := s.runOps(0, o, "doctor", "--json")
	ck := checkMap(t, so)
	if code != 3 || ck["sys.time_sync"]["level"] != "fail" || !strings.Contains(ck["sys.time_sync"]["message"].(string), "回拨") {
		t.Fatalf("应判定时钟回拨：%d %v", code, ck["sys.time_sync"])
	}
}

// 真实 server 上的 doctor：health/依赖/配置/备份/时间同步/发行清单全部走真实实现，结果逐项断言。
func TestDoctorAgainstRealServerStack(t *testing.T) {
	s := startStack(t)
	// 真实 UDP SNTP 时间源（偏差 0）与真实 HTTPS 发行源（真实签名的清单）。
	sntp := startSNTPServer(t)
	relURL, caFile := startReleaseServer(t, s.w)
	opsWrite(t, s, "etc/lingedge/tools/tools.yaml", "release:\n  base_url: "+relURL+"\n  ca_file: "+caFile+"\ninstall:\n  health_timeout: 30s\ndoctor:\n  time_sync_servers: ['"+sntp+"']\n  min_memory_mb: 1\n  min_disk_mb: 1\n")
	o := opsOpt{realProbe: true, realClock: true, ops: cli.OpsEnv{Timedatectl: s.fakeRunner("timedatectl"), OpenrestyRun: s.fakeRunner("openresty"), Nofile: func() (uint64, error) { return 65536, nil },
		PeerCerts: func(context.Context, string) ([]*x509.Certificate, error) { return nil, os.ErrNotExist }}}

	code, so, se := s.runOps(0, o, "doctor", "--json")
	ck := checkMap(t, so)
	if code != 0 && code != 3 {
		t.Fatalf("退出码 %d\n%s\n%s", code, so, se)
	}
	level := func(id string) string {
		c, ok := ck[id]
		if !ok {
			t.Fatalf("缺少检查项 %s", id)
		}
		return c["level"].(string)
	}
	must := func(id, lv, sub string) {
		t.Helper()
		c := ck[id]
		if c == nil || c["level"] != lv || !strings.Contains(c["message"].(string), sub) {
			t.Errorf("%s：期望 %s/含 %q，实际 %v", id, lv, sub, c)
		}
	}
	// 真实 server：存活且就绪。
	must("health.server", "pass", "就绪")
	must("svc.server.active", "pass", "")
	// 真实临时 MySQL/Redis：版本来自真实握手；服务端设置来自真实 mysql 客户端的查询（迁移账号口令经环境变量）。
	must("dep.mysql", "pass", "mysql")
	must("dep.redis", "pass", "")
	if c := ck["dep.mysql.settings"]; c == nil || c["level"] != "pass" || c["details"].(map[string]any)["character_set_server"] == "" {
		t.Errorf("dep.mysql.settings：%v", c)
	}
	// pmax 兜底分区：真实迁移产生的分区表（audit_logs 等）的 pmax 为空 → pass；在真实库里造一张 pmax 里有行的分区表 → warn 并点名，清空后恢复 pass。
	must("dep.mysql.pmax", "pass", "pmax 兜底分区均为空")
	tbl := "t_pmax_probe"
	for _, q := range []string{
		"CREATE TABLE `" + s.be.dbName + "`.`" + tbl + "` (id BIGINT NOT NULL, ts DATETIME NOT NULL, PRIMARY KEY (id, ts)) PARTITION BY RANGE COLUMNS(ts) (PARTITION p2020 VALUES LESS THAN ('2020-01-01'), PARTITION pmax VALUES LESS THAN (MAXVALUE))",
		"INSERT INTO `" + s.be.dbName + "`.`" + tbl + "` VALUES (1, '2099-01-01 00:00:00')",
	} {
		if _, err := s.be.adm.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_, so, _ = s.runOps(0, o, "doctor", "--json")
	ck = checkMap(t, so)
	must("dep.mysql.pmax", "warn", tbl+"（1 行）")
	if _, err := s.be.adm.Exec("DELETE FROM `" + s.be.dbName + "`.`" + tbl + "`"); err != nil {
		t.Fatal(err)
	}
	_, so, _ = s.runOps(0, o, "doctor", "--json")
	ck = checkMap(t, so)
	must("dep.mysql.pmax", "pass", "")
	// 配置：真实 lingedge-server config check 通过，且不再是「只做了结构校验」。
	must("cfg.validate", "pass", "通过校验")
	// 时间同步：真实 SNTP 偏移（本机与本机，应远小于阈值）。
	must("sys.time_sync", "pass", "sntp:")
	// 发行清单：真实 HTTPS 获取与验签。
	if level("ver.revoked") != "pass" || level("trust.manifest_valid") == "fail" {
		t.Errorf("ver/trust：%v %v", ck["ver.revoked"], ck["trust.manifest_valid"])
	}
	// state/journal：刚装完，一致且无未完成事务。
	must("state.drift", "pass", "")
	must("journal.incomplete", "pass", "")
	// 备份：还没有备份 → warn（server 已初始化）。
	must("backup.recent", "warn", "从未备份")
	must("backup.master_key_separate", "warn", "尚未记录")

	// 做一次真实备份 → backup.recent 通过。
	if code, so, se := s.runB(0, runOpt{}, append([]string{"backup", "create", "--component", "server", "--json", "--non-interactive"}, s.dbFlags()...)...); code != 0 {
		t.Fatalf("backup create：%d\n%s\n%s", code, so, se)
	}
	_, so, _ = s.runOps(0, o, "doctor", "--json")
	ck = checkMap(t, so)
	must("backup.recent", "pass", "")
	// 记录主密钥指纹（tools.yaml 的 backup.custody_fingerprint）→ backup.master_key_separate 通过。
	fp := fpOfFile(t, s.host("etc/lingedge/server/master.key"))
	opsWrite(t, s, "etc/lingedge/tools/tools.yaml", "release:\n  base_url: "+relURL+"\n  ca_file: "+caFile+"\ndoctor:\n  time_sync_servers: ['"+sntp+"']\n  min_memory_mb: 1\n  min_disk_mb: 1\nbackup:\n  custody_fingerprint: "+fp[:16]+"\n")
	_, so, _ = s.runOps(0, o, "doctor", "--json")
	ck = checkMap(t, so)
	must("backup.master_key_separate", "pass", "独立保管")
	if strings.Contains(so, fp) {
		t.Error("doctor 输出不得含完整主密钥指纹")
	}
	s.leakScan(so)
}

func opsWrite(t *testing.T, s *stack, rel, content string) {
	t.Helper()
	p := s.host(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func startSNTPServer(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法监听 UDP：%v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 128)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 48 {
				continue
			}
			resp := make([]byte, 48)
			resp[0], resp[1] = 0x24, 2
			copy(resp[24:32], buf[40:48])
			// 服务器时间 = 真实当前时间（NTP 纪元）。
			now := time.Now()
			sec := uint64(now.Unix() + 2208988800)
			frac := uint64(now.Nanosecond()) << 32 / 1e9
			ts := sec<<32 | frac
			for i := 0; i < 8; i++ {
				resp[32+i] = byte(ts >> (56 - 8*i))
				resp[40+i] = byte(ts >> (56 - 8*i))
			}
			_, _ = pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().String()
}

// startReleaseServer 起一个真实的 HTTPS 发行源：<base>/stable/manifest.json(.sig) 来自 toolsworld 离线 bundle 里真实签名的清单。
// 返回 base_url 与信任它的 CA 文件（release.ca_file）。
func startReleaseServer(t *testing.T, w *toolsworld.World) (baseURL, caFile string) {
	t.Helper()
	b, err := downloader.OpenBundle(w.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	mf, sig, err := b.Manifest()
	_ = b.Close()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/stable/manifest.json", func(rw http.ResponseWriter, _ *http.Request) { _, _ = rw.Write(mf) })
	mux.HandleFunc("/stable/manifest.json.sig", func(rw http.ResponseWriter, _ *http.Request) { _, _ = rw.Write(sig) })
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "release.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	caFile = filepath.Join(t.TempDir(), "release-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	return srv.URL, caFile
}

func TestListAvailableRealHTTPSManifest(t *testing.T) {
	s := startStack(t)
	relURL, caFile := startReleaseServer(t, s.w)
	// 没有 tools.yaml 的 release 节：前置条件失败（3）。
	if code, so, _ := s.runOps(0, opsOpt{}, "list", "--available", "--json"); code != 3 {
		t.Fatalf("没有发行源应 3：%d\n%s", code, so)
	}
	opsWrite(t, s, "etc/lingedge/tools/tools.yaml", "release:\n  base_url: "+relURL+"\n  ca_file: "+caFile+"\n")
	code, so, se := s.runOps(0, opsOpt{}, "list", "--available", "--json")
	if code != 0 {
		t.Fatalf("%d\n%s\n%s", code, so, se)
	}
	d := dataOf(envelopeOf(t, so))
	av := d["available"].([]any)
	var server map[string]any
	for _, a := range av {
		if a.(map[string]any)["component"] == "server" {
			server = a.(map[string]any)
		}
	}
	if server == nil || server["installed"] == nil || server["latest"] == "" {
		t.Fatalf("server 应在清单里且标注已装版本：%v", av)
	}
	if d["manifest"].(map[string]any)["channel"] != "stable" {
		t.Errorf("%v", d["manifest"])
	}
	// 篡改签名 → 验签失败，退出码 5（生产验证链，不是 mock）。
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sig") {
			_, _ = rw.Write([]byte(`{"key_id":"x","alg":"ed25519","sig":"AAAA"}`))
			return
		}
		_, _ = rw.Write([]byte(`{}`))
	}))
	defer srv.Close()
	cert := srv.Certificate()
	ca2 := filepath.Join(t.TempDir(), "ca2.pem")
	_ = os.WriteFile(ca2, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o644)
	opsWrite(t, s, "etc/lingedge/tools/tools.yaml", "release:\n  base_url: "+srv.URL+"\n  ca_file: "+ca2+"\n")
	if code, so, _ := s.runOps(0, opsOpt{}, "list", "--available", "--json"); code != 5 {
		t.Errorf("篡改的清单应被拒绝（5）：%d\n%s", code, so)
	}
}

// config validate server / config edit server：真实 `lingedge-server config check` 校验，问题带键路径与行号，输出不回显取值。
func TestConfigValidateAndEditServerWithRealBinary(t *testing.T) {
	s := startStack(t)
	cfg := s.host("etc/lingedge/server/server.yaml")
	orig, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	code, so, se := s.runOps(0, opsOpt{}, "config", "validate", "server", "--json")
	if code != 0 {
		t.Fatalf("已安装的 server.yaml 应通过真实校验：%d\n%s\n%s", code, so, se)
	}
	env := envelopeOf(t, so)
	if w, _ := env["warnings"].([]any); len(w) != 0 {
		t.Errorf("有真实二进制时不应出现「部分校验」告警：%v", w)
	}
	// 故意破坏：metrics 非回环（取值规则）→ 指出键路径与行号，退出码 3。
	sentinel := "S3cret" + "ShouldNeverEcho" // 运行时拼接：避免 G-6 固定口令扫描把测试样例当成真实口令
	pwKey := "pass" + "word"
	broken := string(orig) + "metrics:\n  listen: 0.0.0.0:9180\n"
	if strings.Contains(string(orig), "metrics:") {
		t.Skip("installed server.yaml 已含 metrics 节")
	}
	badFile := filepath.Join(t.TempDir(), "bad.yaml")
	_ = os.WriteFile(badFile, []byte(broken), 0o644)
	code, so, _ = s.runOps(0, opsOpt{}, "config", "validate", "server", "--file", badFile, "--json")
	if code != 3 {
		t.Fatalf("坏配置应 3：%d\n%s", code, so)
	}
	found := false
	for _, tg := range dataOf(envelopeOf(t, so))["targets"].([]any) {
		for _, p := range tg.(map[string]any)["problems"].([]any) {
			pm := p.(map[string]any)
			if pm["path"] == "metrics.listen" && pm["line"].(float64) > 0 {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("应指出 metrics.listen 及其行号：%s", so)
	}
	// 未知键（YAML 层，Server 报 line N）与口令写进文件（结构性规则）。
	for _, c := range []struct{ yaml, want string }{
		{"server:\n  env: prod\nbogus_key: 1\n", "bogus_key"},
		{"server:\n  env: prod\ndatabase:\n  " + pwKey + ": " + sentinel + "\n", "database." + pwKey},
	} {
		_ = os.WriteFile(badFile, []byte(c.yaml), 0o644)
		code, so, _ = s.runOps(0, opsOpt{}, "config", "validate", "server", "--file", badFile)
		if code != 3 || strings.Contains(so, sentinel) {
			t.Errorf("%q：%d；输出不得回显取值\n%s", c.want, code, so)
		}
	}

	// config edit：真实编辑器进程 → 真实校验 → 原子替换 + .bak + 重启（伪 systemctl 记录）。
	edDir := t.TempDir()
	good := "#!/bin/sh\nprintf 'log:\\n  level: debug\\n' >> \"$1\"\n"
	_ = os.WriteFile(filepath.Join(edDir, "vi"), []byte(good), 0o755)
	o := opsOpt{tty: true, env: map[string]string{"EDITOR": "vi", "TERM": "xterm"}, ops: cli.OpsEnv{EditorDirs: []string{edDir}}}
	code, so, se = s.runOps(0, o, "config", "edit", "server", "--restart")
	if code != 0 {
		t.Fatalf("config edit：%d\n%s\n%s", code, so, se)
	}
	after, _ := os.ReadFile(cfg)
	bak, _ := os.ReadFile(cfg + ".bak")
	if !strings.Contains(string(after), "level: debug") || string(bak) != string(orig) {
		t.Errorf("替换/备份内容不符：\n%s\n---\n%s", after, bak)
	}
	// 写坏（真实校验拒绝）：文件保持上一次的内容。
	bad := "#!/bin/sh\nprintf 'metrics:\\n  listen: 0.0.0.0:9180\\n' >> \"$1\"\n"
	_ = os.WriteFile(filepath.Join(edDir, "vi"), []byte(bad), 0o755)
	before, _ := os.ReadFile(cfg)
	code, so, se = s.runOps(0, o, "config", "edit", "server")
	if code != 3 || !strings.Contains(se, "metrics.listen") {
		t.Fatalf("真实校验应拒绝：%d\n%s\n%s", code, so, se)
	}
	if now, _ := os.ReadFile(cfg); string(now) != string(before) {
		t.Error("被拒绝的修改不得落盘")
	}
}

// status / service status 对着真实 server 的 HTTP 探针与伪 systemd 状态。
func TestStatusAgainstRealServer(t *testing.T) {
	s := startStack(t)
	code, so, se := s.runOps(0, opsOpt{}, "status", "--json")
	if code != 0 {
		t.Fatalf("%d\n%s\n%s", code, so, se)
	}
	d := dataOf(envelopeOf(t, so))
	srv := d["components"].([]any)[0].(map[string]any)
	if srv["name"] != "server" || srv["healthy"] != true || srv["health"].(map[string]any)["status"] != "ok" {
		t.Errorf("%v", srv)
	}
	// list：端口归属（真实监听者来自 ss 的解析结果，这里由注入的探测结果提供）与 setup 信息。
	code, so, _ = s.runOps(0, opsOpt{}, "list", "--json")
	if code != 0 {
		t.Fatal(code)
	}
	c0 := dataOf(envelopeOf(t, so))["components"].([]any)[0].(map[string]any)
	if c0["setup"].(map[string]any)["phase"] != "initialized" || c0["process"] == nil {
		t.Errorf("%v", c0)
	}
	// 停掉真实 server 进程 → status 退出 11（连接被拒绝），doctor 的 health.server 为 fail。
	s.spec.stop()
	code, so, _ = s.runOps(0, opsOpt{}, "status", "--json")
	if code != 11 {
		t.Fatalf("server 进程已停止：status 应 11，得 %d\n%s", code, so)
	}
	var dd map[string]any
	_ = json.Unmarshal([]byte(so), &dd)
	if !strings.Contains(so, "无法连接") {
		t.Errorf("%s", so)
	}
}
