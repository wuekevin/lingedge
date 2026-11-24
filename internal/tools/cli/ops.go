package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/doctor"
	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// StreamRunner 是需要把子进程输出实时转发的执行入口（journalctl 跟随、交互式编辑器）；*safeexec.Runner 实现了它。
type StreamRunner interface {
	Stream(ctx context.Context, c safeexec.Cmd, stdout, stderr io.Writer) error
}

// OpsEnv 是运维命令（doctor/config/service/logs/status/list）的外部依赖装配（Env.Ops）。零值 = 生产。
// 与 InstallEnv/BackupEnv 同一原则：它们是「世界」的接缝，业务逻辑（检查项、校验、脱敏、原子替换）永远是生产代码。
// systemctl/探测/根目录前缀/属主复用 Env.Install（同一个世界）。
type OpsEnv struct {
	// HTTPGet 对本机 URL 发 GET 返回状态码；nil = 真实 HTTP 客户端（3 秒超时，不跟随重定向）。
	HTTPGet func(ctx context.Context, url string) (int, error)
	// Journal 执行 journalctl；nil = 经 safeexec 的真实 journalctl（固定目录解析）。
	Journal StreamRunner
	// Editor 执行交互式编辑器；nil = 经 safeexec 的真实编辑器（白名单：vi/vim/nano/editor/emacs，固定目录解析）。
	Editor StreamRunner
	// Timedatectl 执行 timedatectl；nil = 真实（仅 timedatectl）。TimeDial 是 SNTP 的 UDP 拨号函数；nil = 真实。
	Timedatectl service.Runner
	TimeDial    func(ctx context.Context, network, addr string) (net.Conn, error)
	// PeerCerts 取得 TLS 对端证书链；nil = 真实握手（只读诊断，不发送数据、不信任对端）。
	PeerCerts func(ctx context.Context, addr string) ([]*x509.Certificate, error)
	// OpenrestyRun 执行 openresty -V；nil = 真实（仅 openresty）。
	OpenrestyRun service.Runner
	// FetchManifest 获取并验证清单（channel：stable|beta）；nil = 生产：tools.yaml 的 release.* + downloader + 内置公钥验证（只读，不推进 sequence）。
	FetchManifest func(ctx context.Context, channel string) (*manifest.Manifest, error)
	// Nofile 返回 RLIMIT_NOFILE 软限制；nil = 真实。
	Nofile func() (uint64, error)
	// MySQLSettings 查询 MySQL 服务端设置；nil = 生产：迁移账号凭据（同 backup）+ mysql 客户端。
	MySQLSettings func(ctx context.Context) (doctor.MySQLSettings, error)
	// PmaxRows 列出有 pmax 分区的表及其行数；nil = 生产：迁移账号凭据 + mysql 客户端。
	PmaxRows func(ctx context.Context) ([]doctor.PartitionRow, error)
	// EditorDirs 是编辑器的搜索目录（nil = 固定系统目录；测试放置脚本编辑器）。
	EditorDirs []string
	// ExtraChecks 是追加的 doctor 检查组（后续 Story：node/dns/disk）。
	ExtraChecks []doctor.Group
}

func (r *Run) ops() OpsEnv { return r.env.Ops }

// ---- tools.yaml ----

func (r *Run) toolsConfigPath() (path string, explicit bool) {
	if r.opts.Config != "" {
		return r.opts.Config, true
	}
	return r.hostPath(DefaultConfigPath), false
}

// loadToolsConfig 严格读取 tools.yaml（不存在 = 缺省）。
func (r *Run) loadToolsConfig() (installer.ToolsConfig, error) {
	p, explicit := r.toolsConfigPath()
	return installer.LoadToolsConfig(p, explicit)
}

// toolsConfigLenient 读取 tools.yaml，任何错误都回落到缺省配置：诊断类命令（doctor/config validate）必须在配置损坏时仍能运行并报告它。
func (r *Run) toolsConfigLenient() installer.ToolsConfig {
	cfg, err := r.loadToolsConfig()
	if err != nil {
		return installer.ToolsConfig{}
	}
	return cfg
}

// ---- 组件视图 ----

func toDoctorComponents(comps []ComponentInfo) []doctor.Component {
	out := make([]doctor.Component, 0, len(comps))
	for _, c := range comps {
		out = append(out, doctor.Component{Name: c.Name, Version: c.Version, State: c.State, Unit: c.Unit, RunUser: c.RunUser, Ports: c.Ports,
			ReleaseDir: c.ReleaseDir, ConfigDir: c.ConfigDir, DataDir: c.DataDir})
	}
	return out
}

// ---- systemctl / 探测 ----

func (r *Run) systemctl() *service.Systemctl {
	ie := r.env.Install
	runner := ie.Runner
	if runner == nil {
		rr, err := service.SystemRunner()
		if err != nil {
			return nil
		}
		runner = rr
	}
	return &service.Systemctl{Run: runner, Booted: ie.Booted}
}

func (r *Run) collect(ctx context.Context, listenersOnly bool) (probe.Result, error) {
	if c := r.env.Install.Collect; c != nil {
		return c(ctx)
	}
	return probe.Collect(ctx, probe.Options{ListenersOnly: listenersOnly})
}

func (r *Run) httpGet(ctx context.Context, url string) (int, error) {
	if f := r.ops().HTTPGet; f != nil {
		return f(ctx, url)
	}
	return realHTTPGet(ctx, url)
}

// realHTTPGet 对（本机）url 发 GET：3 秒超时、不跟随重定向、不读取响应体（只要状态码）。
func realHTTPGet(ctx context.Context, url string) (int, error) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	cl := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, errors.New(sanitizeNetErr(err))
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

func sanitizeNetErr(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "连接超时"
	case errors.As(err, &ne) && ne.Timeout():
		return "连接超时"
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		return "连接失败（" + oe.Err.Error() + "）"
	}
	return "连接失败"
}

// realPeerCerts 对 addr 做一次 TLS 握手并返回对端证书链。只读诊断：不发送任何应用数据、不信任对端、不据此做任何安全决定——
// 目的仅是读取 Server gRPC 叶证书/CA 的有效期（私有 CA 签发，本机并不信任它）。因此握手不校验链，但会在 VerifyPeerCertificate 里取走证书后立刻关闭连接。
func realPeerCerts(ctx context.Context, addr string) ([]*x509.Certificate, error) {
	d := net.Dialer{Timeout: 3 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, errors.New(sanitizeNetErr(err))
	}
	defer func() { _ = raw.Close() }()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	var got []*x509.Certificate
	conf := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // 仅读取证书用于有效期诊断；见函数注释
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			for _, rc := range rawCerts {
				if c, perr := x509.ParseCertificate(rc); perr == nil {
					got = append(got, c)
				}
			}
			return nil
		},
	}
	conn := tls.Client(raw, conf)
	// 服务端要求客户端证书时握手会在之后失败，但证书链已经取到了；只在什么都没取到时才算失败。
	herr := conn.HandshakeContext(ctx)
	if len(got) == 0 {
		if herr != nil {
			return nil, errors.New("TLS 握手失败")
		}
		return nil, errors.New("对端没有出示证书")
	}
	return got, nil
}

// ---- doctor 的 World ----

func (r *Run) serverListens() (httpListen, grpcListen string) {
	httpListen, grpcListen = "127.0.0.1:8080", "0.0.0.0:9443"
	b, err := os.ReadFile(r.hostPath(backup.ServerConfigFile))
	if err != nil {
		return
	}
	var shape struct {
		HTTP struct {
			Listen string `yaml:"listen"`
		} `yaml:"http"`
		GRPC struct {
			Listen string `yaml:"listen"`
		} `yaml:"grpc"`
	}
	if err := yamlLenient(b, &shape); err != nil {
		return
	}
	if shape.HTTP.Listen != "" {
		httpListen = shape.HTTP.Listen
	}
	if shape.GRPC.Listen != "" {
		grpcListen = shape.GRPC.Listen
	}
	return
}

func (r *Run) doctorWorld(ctx context.Context, fix bool) (*doctor.World, installer.ToolsConfig) {
	ie, oe := r.env.Install, r.ops()
	comps, serr := r.env.State.Components(ctx)
	tc := r.toolsConfigLenient()
	cfg := doctor.Config{
		MinMemoryMB: int64(tc.Doctor.MinMemoryMB), MinDiskMB: int64(tc.Doctor.MinDiskMB), NofileMin: uint64(tc.Doctor.NofileMin), RestartsWarn: tc.Doctor.RestartsWarn,
		BackupDir: tc.Backup.Dir, BackupMaxAge: tc.BackupRecentMaxAge(), CustodyFingerprint: tc.Backup.CustodyFingerprint, Offline: tc.Doctor.Offline,
	}
	cfg.TimeSkewWarn, _ = tc.TimeSkewWarn()
	cfg.TimeSkewFail, _ = tc.TimeSkewFail()
	if cfg.BackupDir == "" {
		cfg.BackupDir = backup.DefaultBackupDir
	}
	own := ie.Own
	if own == nil {
		own = service.SystemOwnership()
	}
	w := &doctor.World{
		Root: ie.Root, Now: r.env.Now, Euid: r.env.Geteuid(), FixPerms: fix, DryRun: r.opts.DryRun,
		Components: toDoctorComponents(comps), StateErr: serr, StateDir: filepath.Dir(r.env.StatePath), Cfg: cfg,
		Probe:     func(ctx context.Context) (probe.Result, error) { return r.collect(ctx, false) },
		Systemctl: r.systemctl(), Own: own,
		HTTPGet: r.httpGet, ServerListens: r.serverListens, FSKind: registry.FSKind,
		PeerCerts: oe.PeerCerts, Nofile: oe.Nofile,
		ConfigVerify: func(ctx context.Context, target string) doctor.ConfigVerdict { return r.verifyTarget(ctx, target, "") },
	}
	if w.PeerCerts == nil {
		w.PeerCerts = realPeerCerts
	}
	if w.Nofile == nil {
		w.Nofile = doctor.NofileLimit
	}
	w.ClockMarks = func() []doctor.ClockMark { return r.clockMarks() }
	w.TimeSync = r.timeSyncSource(tc)
	w.Journals = func(ctx context.Context) ([]registry.TxInfo, error) {
		reg := r.registry()
		if !reg.L.Exists() {
			return nil, nil
		}
		return reg.ListJournals(ctx)
	}
	w.Backups = func() ([]backup.ListEntry, error) {
		bs, berr := r.backupServiceForDoctor()
		if berr != nil {
			return nil, berr
		}
		dir := tc.Backup.Dir
		if dir == "" {
			dir = bs.DefaultDir()
		} else {
			dir = r.hostPath(dir)
		}
		return bs.List(dir)
	}
	w.MasterKeyFingerprint = func() (string, error) {
		ks, kerr := backup.LoadKeySet(r.hostPath(backup.ServerMasterKey), r.env.Backup.AllowedUIDs)
		if kerr != nil {
			return "", kerr
		}
		return ks.Overall, nil
	}
	w.Manifest = r.doctorManifest(tc)
	w.MySQLSettings = r.doctorMySQL()
	w.PmaxRows = r.doctorPmax()
	w.OpenrestyV = r.openrestyV
	return w, tc
}

// clockMarks 是 Tools 自己记录过的时间点：state.json 的修改时间与内部 updated_at、信任库 sequence 文件、日志文件、最近的 journal。
func (r *Run) clockMarks() []doctor.ClockMark {
	var out []doctor.ClockMark
	st := registry.LayoutForStatePath(r.env.StatePath)
	add := func(name, p string) {
		if fi, err := os.Stat(p); err == nil {
			out = append(out, doctor.ClockMark{Name: name, T: fi.ModTime()})
		}
	}
	add("state.json 的修改时间", st.StateFile())
	add("信任库 sequence 的修改时间", st.SequenceFile())
	if b, err := os.ReadFile(st.StateFile()); err == nil {
		if t, ok := stateUpdatedAt(b); ok {
			out = append(out, doctor.ClockMark{Name: "state.json 的 updated_at", T: t})
		}
	}
	if ents, err := os.ReadDir(st.JournalDir()); err == nil {
		var newest time.Time
		for _, e := range ents {
			if fi, ferr := e.Info(); ferr == nil && fi.Mode().IsRegular() && fi.ModTime().After(newest) {
				newest = fi.ModTime()
			}
		}
		if !newest.IsZero() {
			out = append(out, doctor.ClockMark{Name: "最近的事务日志", T: newest})
		}
	}
	return out
}

func (r *Run) timeSyncSource(tc installer.ToolsConfig) func(ctx context.Context) (doctor.TimeInfo, error) {
	oe := r.ops()
	return func(ctx context.Context) (doctor.TimeInfo, error) {
		src := doctor.SystemTimeSync{Servers: tc.Doctor.TimeSyncServers, Offline: tc.Doctor.Offline, Now: r.env.Now, Dial: oe.TimeDial}
		run := oe.Timedatectl
		if run == nil {
			if rr, err := doctor.TimedatectlRunner(nil); err == nil {
				run = rr
			}
		}
		src.Runner = run
		return src.Info(ctx)
	}
}

func (r *Run) openrestyV(ctx context.Context) (string, error) {
	run := r.ops().OpenrestyRun
	if run == nil {
		rr, err := service.SystemWebServerRunner()
		if err != nil {
			return "", err
		}
		run = rr
	}
	res, err := run.Run(ctx, safeexec.Cmd{Path: "openresty", Args: []string{"-V"}})
	out := string(res.Stdout) + string(res.Stderr) // openresty -V 把版本与编译参数写到 stderr
	if err != nil && out == "" {
		return "", fmt.Errorf("无法执行 openresty -V")
	}
	return out, nil
}

func (r *Run) backupServiceForDoctor() (*backup.Service, error) {
	bs, e := r.backupService(nil)
	if e != nil {
		return nil, e
	}
	return bs, nil
}
