package doctor

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
)

func TestSysOSArchTable(t *testing.T) {
	cases := []struct {
		os, arch       string
		wantOS, wantAr Level
	}{
		{"linux", "amd64", LevelPass, LevelPass}, {"linux", "arm64", LevelPass, LevelPass},
		{"darwin", "arm64", LevelFail, LevelPass}, {"linux", "riscv64", LevelPass, LevelFail}, {"windows", "386", LevelFail, LevelFail},
	}
	for _, c := range cases {
		w := newWorld(t)
		s := goodSystem()
		s.OS, s.Arch = c.os, c.arch
		w.Probe = fakeProbe(s)
		rep := runAll(t, w, Filter{})
		want(t, find(t, rep, "sys.os"), c.wantOS, "")
		want(t, find(t, rep, "sys.arch"), c.wantAr, "")
	}
}

func TestSysMemAndDiskThresholds(t *testing.T) {
	cases := []struct {
		name      string
		avail     int64
		free      int64
		cfg       Config
		mem, disk Level
	}{
		{"充足", 4000, 100000, Config{}, LevelPass, LevelPass},
		{"内存略低于缺省 256", 200, 100000, Config{}, LevelWarn, LevelPass},
		{"内存远低于缺省", 100, 100000, Config{}, LevelFail, LevelPass},
		{"磁盘低于缺省 512", 4000, 400, Config{}, LevelPass, LevelWarn},
		{"磁盘远低于缺省", 4000, 100, Config{}, LevelPass, LevelFail},
		{"配置抬高内存下限", 1500, 100000, Config{MinMemoryMB: 2048}, LevelWarn, LevelPass},
		{"恰好等于下限不告警", 256, 512, Config{}, LevelPass, LevelPass},
	}
	for _, c := range cases {
		w := newWorld(t)
		s := goodSystem()
		s.MemoryAvailableMB = c.avail
		s.Disks = []resolver.Disk{{Mount: "/", FreeMB: c.free}}
		w.Probe, w.Cfg = fakeProbe(s), c.cfg
		rep := runAll(t, w, Filter{})
		want(t, find(t, rep, "sys.mem"), c.mem, "")
		want(t, find(t, rep, "sys.disk"), c.disk, "")
	}
}

func TestSysDiskPerMountAndReadOnly(t *testing.T) {
	w := newWorld(t)
	s := goodSystem()
	s.Disks = []resolver.Disk{{Mount: "/", FreeMB: 100000}, {Mount: "/var/lib", FreeMB: 100}}
	w.Probe = fakeProbe(s)
	f := find(t, runAll(t, w, Filter{}), "sys.disk")
	want(t, f, LevelFail, "100")
	s.Disks = []resolver.Disk{{Mount: "/", FreeMB: 100000, ReadOnly: true}}
	w2 := newWorld(t)
	w2.Probe = fakeProbe(s)
	want(t, find(t, runAll(t, w2, Filter{}), "sys.disk"), LevelFail, "只读")
}

// 清单声明的系统要求优先于 tools.yaml 的下限；低于清单要求 = fail（而不是 warn）。
func TestSysRequirementsFromManifest(t *testing.T) {
	w := newWorld(t)
	s := goodSystem()
	s.MemoryAvailableMB = 1500
	w.Probe = fakeProbe(s)
	w.Components = []Component{{Name: "server", Version: "1.0.0"}}
	w.Manifest = func(context.Context) (*manifest.Manifest, error) {
		return &manifest.Manifest{Components: map[string]manifest.Component{"server": {Status: "available", Versions: []manifest.VersionEntry{
			{Version: "1.0.0", Variants: map[string]manifest.Variant{"default": {Requires: &manifest.Requires{System: &manifest.SystemReq{MinMemoryMB: 2048, MinDiskMB: 100}}}}}}}}}, nil
	}
	rep := runAll(t, w, Filter{})
	f := find(t, rep, "sys.mem")
	want(t, f, LevelFail, "清单要求")
	if f.Details["required_source"] != "清单声明的系统要求" {
		t.Errorf("%+v", f.Details)
	}
	want(t, find(t, rep, "sys.disk"), LevelPass, "")
}

func TestProbeUnavailableSkips(t *testing.T) {
	w := newWorld(t)
	w.Probe = func(context.Context) (probe.Result, error) { return probe.Result{}, errors.New("boom") }
	rep := runAll(t, w, Filter{})
	for _, id := range []string{"sys.os", "sys.arch", "sys.mem", "sys.disk"} {
		want(t, find(t, rep, id), LevelSkip, "boom")
	}
}

func TestNofile(t *testing.T) {
	w := newWorld(t)
	w.Nofile = func() (uint64, error) { return 1024, nil }
	want(t, find(t, runAll(t, w, Filter{}), "sys.ulimit_nofile"), LevelWarn, "1024")
	w.Nofile = func() (uint64, error) { return 65536, nil }
	want(t, find(t, runAll(t, newWorldWith(w), Filter{}), "sys.ulimit_nofile"), LevelPass, "")
	w.Nofile = func() (uint64, error) { return 0, errors.New("x") }
	want(t, find(t, runAll(t, newWorldWith(w), Filter{}), "sys.ulimit_nofile"), LevelSkip, "")
}

// newWorldWith 复制 w（World 含 sync.Once，不能按值复制，这里只复制测试用到的字段）。
func newWorldWith(w *World) *World {
	return &World{Root: w.Root, Now: w.Now, Euid: w.Euid, Own: w.Own, StateDir: w.StateDir, Nofile: w.Nofile, Probe: w.Probe, Components: w.Components, Cfg: w.Cfg}
}

// ---- sys.time_sync ----

func bp(b bool) *bool { return &b }
func dp(d time.Duration) *time.Duration {
	return &d
}

func TestEvalTimeSyncTable(t *testing.T) {
	cfg := Config{}.withDefaults() // warn 5s, fail 5m
	now := t0
	cases := []struct {
		name  string
		info  TimeInfo
		err   error
		marks []ClockMark
		want  Level
		sub   string
	}{
		{"同步且偏移很小", TimeInfo{Synchronized: bp(true), Offset: dp(40 * time.Millisecond), OffsetSource: "sntp:x"}, nil, nil, LevelPass, "40"},
		{"偏移 6s 告警", TimeInfo{Offset: dp(6 * time.Second), OffsetSource: "sntp:x"}, nil, nil, LevelWarn, "6.0 秒"},
		{"本机偏慢 -6s 同样告警", TimeInfo{Offset: dp(-6 * time.Second), OffsetSource: "sntp:x"}, nil, nil, LevelWarn, ""},
		{"偏移 6 分钟失败", TimeInfo{Offset: dp(6 * time.Minute), OffsetSource: "sntp:x"}, nil, nil, LevelFail, "6.0 分钟"},
		{"恰好等于告警阈值也告警", TimeInfo{Offset: dp(5 * time.Second), OffsetSource: "sntp:x"}, nil, nil, LevelWarn, ""},
		{"未同步", TimeInfo{Synchronized: bp(false), SyncSource: "timedatectl"}, nil, nil, LevelWarn, "未与时间源同步"},
		{"已同步无偏移信息", TimeInfo{Synchronized: bp(true)}, nil, nil, LevelPass, ""},
		{"两种来源都没有", TimeInfo{}, nil, nil, LevelSkip, "doctor.time_sync_servers"},
		{"来源出错", TimeInfo{}, errors.New("坏了"), nil, LevelSkip, "坏了"},
		{"时钟回拨 1 小时", TimeInfo{Synchronized: bp(true)}, nil, []ClockMark{{Name: "state.json 的 updated_at", T: now.Add(time.Hour)}}, LevelFail, "回拨"},
		{"记录的时间点略晚于现在（<容差）不算回拨", TimeInfo{Synchronized: bp(true)}, nil, []ClockMark{{Name: "x", T: now.Add(4 * time.Minute)}}, LevelPass, ""},
		{"记录时间在过去正常", TimeInfo{Synchronized: bp(true)}, nil, []ClockMark{{Name: "x", T: now.Add(-48 * time.Hour)}}, LevelPass, ""},
		{"回拨优先于同步正常", TimeInfo{Synchronized: bp(true), Offset: dp(0)}, nil, []ClockMark{{Name: "最近的事务日志", T: now.Add(10 * time.Minute)}}, LevelFail, "最近的事务日志"},
		{"同步但偏移大 → 偏移判定", TimeInfo{Synchronized: bp(true), Offset: dp(10 * time.Second), OffsetSource: "sntp:x"}, nil, nil, LevelWarn, ""},
	}
	for _, c := range cases {
		f := evalTimeSync(now, cfg, c.info, c.err, c.marks)
		want(t, f, c.want, c.sub)
		if f.ID != "sys.time_sync" {
			t.Errorf("%s：id=%s", c.name, f.ID)
		}
	}
}

func TestParseNTPSynchronized(t *testing.T) {
	for in, want := range map[string]bool{"NTPSynchronized=yes\n": true, "NTPSynchronized=no": false, "Foo=bar\nNTPSynchronized=yes": true} {
		got, err := parseNTPSynchronized(in)
		if err != nil || got != want {
			t.Errorf("%q → %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "NTPSynchronized=maybe", "NTP=yes"} {
		if _, err := parseNTPSynchronized(in); err == nil {
			t.Errorf("%q 应出错", in)
		}
	}
}

// 真实 UDP 套接字上的 SNTP 服务器：偏移 = 服务器时钟 − 本机时钟的相反数；伪造/不匹配的应答被拒绝。
func startSNTP(t *testing.T, serverNow func() time.Time, mutate func(resp []byte)) string {
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
			resp[0] = 0x24 // LI=0 VN=4 Mode=4
			resp[1] = 2    // stratum 2
			copy(resp[24:32], buf[40:48])
			ts := toNTP(serverNow())
			binary.BigEndian.PutUint64(resp[32:], ts)
			binary.BigEndian.PutUint64(resp[40:], ts)
			if mutate != nil {
				mutate(resp)
			}
			_, _ = pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().String()
}

func TestSNTPOffsetAgainstRealUDPServer(t *testing.T) {
	// 本机时钟 = 真实时间；服务器时钟快 90 秒 → 本机相对服务器偏慢 90 秒（负偏移）。
	addr := startSNTP(t, func() time.Time { return time.Now().Add(90 * time.Second) }, nil)
	off, err := SNTPOffset(context.Background(), addr, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if off > -85*time.Second || off < -95*time.Second {
		t.Errorf("偏移 %v，期望约 -90s", off)
	}
	// 本机偏快：服务器慢 7 秒。
	addr = startSNTP(t, func() time.Time { return time.Now().Add(-7 * time.Second) }, nil)
	off, err = SNTPOffset(context.Background(), addr, time.Now, nil)
	if err != nil || off < 6*time.Second || off > 8*time.Second {
		t.Errorf("偏移 %v err=%v，期望约 +7s", off, err)
	}
}

func TestSNTPRejectsBadResponses(t *testing.T) {
	cases := map[string]func(resp []byte){
		"origin 不匹配":      func(r []byte) { r[24] ^= 0xff },
		"stratum 0（kiss）": func(r []byte) { r[1] = 0 },
		"不是 server 模式":    func(r []byte) { r[0] = 0x23 },
		"时间戳为 0":          func(r []byte) { binary.BigEndian.PutUint64(r[32:], 0); binary.BigEndian.PutUint64(r[40:], 0) },
	}
	for name, mut := range cases {
		addr := startSNTP(t, time.Now, mut)
		if _, err := SNTPOffset(context.Background(), addr, time.Now, nil); err == nil {
			t.Errorf("%s：应被拒绝", name)
		}
	}
	// 服务器不应答 → 超时报错而不是挂住。
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = pc.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := SNTPOffset(ctx, pc.LocalAddr().String(), time.Now, nil); err == nil || time.Since(start) > 4*time.Second {
		t.Errorf("无应答应在期限内失败：%v", err)
	}
}

func TestSystemTimeSyncUsesFirstWorkingServer(t *testing.T) {
	good := startSNTP(t, time.Now, nil)
	s := SystemTimeSync{Servers: []string{"127.0.0.1:1", good}, Now: time.Now}
	info, err := s.Info(context.Background())
	if err != nil || info.Offset == nil || info.OffsetSource != "sntp:"+good {
		t.Fatalf("%+v %v", info, err)
	}
	if len(info.Notes) != 1 {
		t.Errorf("第一个不可达的服务器应记入 Notes：%v", info.Notes)
	}
	s.Offline = true
	info, _ = s.Info(context.Background())
	if info.Offset != nil {
		t.Error("offline 不得联网")
	}
}
