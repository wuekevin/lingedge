package doctor

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// TimeInfo 是时间同步状态的原始事实（evalTimeSync 把它与阈值、Tools 自己的时间记录合成一个检查结果）。
type TimeInfo struct {
	// Synchronized：系统是否报告已与时间源同步（timedatectl 的 NTPSynchronized）；nil = 未知。
	Synchronized *bool
	SyncSource   string
	// Offset：本机时钟 − 参考时钟（正 = 本机偏快）；nil = 未测量。
	Offset       *time.Duration
	OffsetSource string
	// Notes 是不阻断的采集问题（例如某个 SNTP 服务器不可达）。
	Notes []string
}

// TimedatectlRunner 是只允许 timedatectl 的执行器（safeexec.DefaultAllowed 不含它：这里单独构造，不放宽默认白名单）。
func TimedatectlRunner(dirs []string) (*safeexec.Runner, error) {
	if len(dirs) == 0 {
		dirs = safeexec.DefaultSearchDirs
	}
	return safeexec.New(safeexec.Options{Allowed: []string{"timedatectl"}, SearchDirs: dirs, MaxOutput: 64 << 10, Timeout: 10 * time.Second})
}

// TimedatectlSynchronized 执行 `timedatectl show --property=NTPSynchronized` 并解析（固定参数数组）。
func TimedatectlSynchronized(ctx context.Context, run service.Runner) (bool, error) {
	res, err := run.Run(ctx, safeexec.Cmd{Path: "timedatectl", Args: []string{"show", "--property=NTPSynchronized"}})
	if err != nil {
		return false, err
	}
	return parseNTPSynchronized(string(res.Stdout))
}

func parseNTPSynchronized(out string) (bool, error) {
	for _, ln := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(ln), "=")
		if !ok || k != "NTPSynchronized" {
			continue
		}
		switch v {
		case "yes":
			return true, nil
		case "no":
			return false, nil
		}
		return false, fmt.Errorf("timedatectl 的 NTPSynchronized 取值无法识别")
	}
	return false, errors.New("timedatectl 没有输出 NTPSynchronized")
}

// ntpEpochOffset 是 NTP 纪元（1900）到 Unix 纪元（1970）的秒数。
const ntpEpochOffset = 2208988800

func toNTP(t time.Time) uint64 {
	s := uint64(t.Unix() + ntpEpochOffset)
	frac := uint64(t.Nanosecond()) << 32 / 1e9
	return s<<32 | frac
}

func fromNTP(v uint64) time.Time {
	s := int64(v>>32) - ntpEpochOffset
	ns := int64((v & 0xffffffff) * 1e9 >> 32)
	return time.Unix(s, ns).UTC()
}

// SNTPOffset 对 server（host:port，UDP）做一次 SNTPv4 查询，返回本机相对服务器的时钟偏移（正 = 本机偏快）。
// 只信任：模式为 server、stratum ≠ 0（排除 kiss-of-death）、origin 时间戳回显了我们发出的值（防止乱序/伪造的旧应答）。
func SNTPOffset(ctx context.Context, server string, now func() time.Time, dial func(ctx context.Context, network, addr string) (net.Conn, error)) (time.Duration, error) {
	if now == nil {
		now = time.Now
	}
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := dial(dctx, "udp", server)
	if err != nil {
		return 0, fmt.Errorf("无法连接时间源 %s", server)
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := dctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	var req [48]byte
	req[0] = 0x23 // LI=0 VN=4 Mode=3(client)
	t1 := now().UTC()
	binary.BigEndian.PutUint64(req[40:], toNTP(t1))
	if _, err := conn.Write(req[:]); err != nil {
		return 0, fmt.Errorf("向时间源 %s 发送请求失败", server)
	}
	var resp [128]byte
	n, err := conn.Read(resp[:])
	t4 := now().UTC()
	if err != nil {
		return 0, fmt.Errorf("时间源 %s 没有应答", server)
	}
	if n < 48 {
		return 0, fmt.Errorf("时间源 %s 的应答过短", server)
	}
	if resp[0]&7 != 4 {
		return 0, fmt.Errorf("时间源 %s 的应答不是 server 模式", server)
	}
	if resp[1] == 0 {
		return 0, fmt.Errorf("时间源 %s 拒绝服务（stratum 0）", server)
	}
	if binary.BigEndian.Uint64(resp[24:]) != binary.BigEndian.Uint64(req[40:]) {
		return 0, fmt.Errorf("时间源 %s 的应答与请求不匹配", server)
	}
	t2 := fromNTP(binary.BigEndian.Uint64(resp[32:]))
	t3 := fromNTP(binary.BigEndian.Uint64(resp[40:]))
	if t2.Year() < 2020 || t3.Year() < 2020 {
		return 0, fmt.Errorf("时间源 %s 的应答时间戳无效", server)
	}
	// 标准 SNTP：offset(服务器 − 本机) = ((T2−T1)+(T3−T4))/2；这里取相反数得到「本机 − 服务器」。
	off := (t2.Sub(t1) + t3.Sub(t4)) / 2
	return -off, nil
}

// SystemTimeSync 是生产的 TimeSync 来源：timedatectl 的同步标志 +（配置了 doctor.time_sync_servers 时）逐个 SNTP 测量，取第一个成功的。
// 两者都不可用时返回空的 TimeInfo（调用方报告 skip）。
type SystemTimeSync struct {
	// Runner 为 nil = 不读 timedatectl。
	Runner  service.Runner
	Servers []string
	// Offline 为 true 时不做 SNTP（doctor.offline）。
	Offline bool
	Now     func() time.Time
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Info 采集 TimeInfo。
func (s SystemTimeSync) Info(ctx context.Context) (TimeInfo, error) {
	var info TimeInfo
	if s.Runner != nil {
		if ok, err := TimedatectlSynchronized(ctx, s.Runner); err == nil {
			info.Synchronized, info.SyncSource = &ok, "timedatectl"
		} else {
			info.Notes = append(info.Notes, "timedatectl 不可用："+err.Error())
		}
	}
	if !s.Offline {
		for _, sv := range s.Servers {
			off, err := SNTPOffset(ctx, sv, s.Now, s.Dial)
			if err != nil {
				info.Notes = append(info.Notes, err.Error())
				continue
			}
			info.Offset, info.OffsetSource = &off, "sntp:"+sv
			break
		}
	}
	return info, nil
}

func fmtDur(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%.1f 小时", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.1f 分钟", d.Minutes())
	case d >= time.Second:
		return fmt.Sprintf("%.1f 秒", d.Seconds())
	}
	return fmt.Sprintf("%d 毫秒", d.Milliseconds())
}

// evalTimeSync 把时间同步事实、时钟回拨判定与阈值合成 sys.time_sync 的结果。
//
// 为什么重要（tools-design §19）：时钟偏移会让清单 valid_until 与证书有效期误判；恢复备份后的 `lineage bump` 取「max(DB 值+1, 当前 UNIX 秒)」，
// 时钟回拨会让之后产生的修订谱系小于节点已应用值。
func evalTimeSync(now time.Time, cfg Config, info TimeInfo, tsErr error, marks []ClockMark) Finding {
	f := Finding{ID: "sys.time_sync", Category: "system"}
	d := map[string]any{}
	// 回拨：当前时钟早于 Tools 自己记录过的任何时间点（容差 = time_skew_fail，抵消文件系统时间戳与 NTP 微调）。
	var worst *ClockMark
	for i := range marks {
		if marks[i].T.IsZero() {
			continue
		}
		if now.Add(cfg.TimeSkewFail).Before(marks[i].T) && (worst == nil || marks[i].T.After(worst.T)) {
			worst = &marks[i]
		}
	}
	if worst != nil {
		back := worst.T.Sub(now)
		f.Level = LevelFail
		f.Message = fmt.Sprintf("系统时钟被回拨：当前时间早于 Tools 记录的「%s」（%s）%s", worst.Name, worst.T.UTC().Format(time.RFC3339), fmtDur(back))
		f.Hint = "先校正系统时钟（chrony/systemd-timesyncd）再执行任何升级/恢复；回拨会让清单 valid_until 与证书有效期误判，并影响恢复后 lineage bump 的单调性"
		d["rewound_by"] = back.String()
		d["mark"] = worst.Name
		f.Details = d
		return f
	}
	if tsErr != nil {
		f.Level, f.Message = LevelSkip, "无法判定时间同步状态："+tsErr.Error()
		return f
	}
	if info.Offset != nil {
		off := *info.Offset
		abs := off
		if abs < 0 {
			abs = -abs
		}
		d["offset_ms"] = off.Milliseconds()
		d["offset_source"] = info.OffsetSource
		switch {
		case abs >= cfg.TimeSkewFail:
			f.Level = LevelFail
			f.Message = fmt.Sprintf("本机时钟与 %s 相差 %s（≥ %s）", info.OffsetSource, fmtDur(off), fmtDur(cfg.TimeSkewFail))
			f.Hint = "启用并核对时间同步（chrony/systemd-timesyncd）；偏差会让清单与证书的有效期判断出错"
		case abs >= cfg.TimeSkewWarn:
			f.Level = LevelWarn
			f.Message = fmt.Sprintf("本机时钟与 %s 相差 %s（≥ %s）", info.OffsetSource, fmtDur(off), fmtDur(cfg.TimeSkewWarn))
			f.Hint = "检查时间同步服务是否正常工作"
		default:
			f.Level = LevelPass
			f.Message = fmt.Sprintf("本机时钟与 %s 相差 %s", info.OffsetSource, fmtDur(off))
		}
	}
	if info.Synchronized != nil {
		d["ntp_synchronized"] = *info.Synchronized
		if !*info.Synchronized {
			if f.Level == "" || f.Level == LevelPass {
				f.Level = LevelWarn
				f.Message = "系统未与时间源同步（timedatectl NTPSynchronized=no）"
				if info.Offset != nil {
					f.Message += fmt.Sprintf("；本机时钟与 %s 相差 %s", info.OffsetSource, fmtDur(*info.Offset))
				}
				f.Hint = "启用 systemd-timesyncd 或 chrony；未同步的时钟会漂移，影响清单/证书有效期判断"
			}
		} else if f.Level == "" {
			f.Level, f.Message = LevelPass, "系统已与时间源同步（timedatectl）；Tools 记录的时间点也没有显示回拨"
		}
	}
	if f.Level == "" {
		f.Level = LevelSkip
		f.Message = "无法判定时间同步状态：没有 timedatectl，也没有配置 doctor.time_sync_servers；未发现时钟回拨迹象"
		f.Hint = "在 tools.yaml 里配置 doctor.time_sync_servers（SNTP 时间源 host:port），或在 systemd 主机上运行"
	}
	if len(info.Notes) > 0 {
		d["notes"] = info.Notes
	}
	if len(d) > 0 {
		f.Details = d
	}
	return f
}
