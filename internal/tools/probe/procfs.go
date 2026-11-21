package probe

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// procfs 读取（根目录可参数化：生产为 /proc，测试用 tests/fixtures 下按真实格式构造的树）。
// 平台无关的纯文件读取；「默认根是 /proc」与「是否在 Linux 上」由平台文件决定。

const (
	maxProcFile = 8 << 20
	maxPIDs     = 200000
	maxFDs      = 100000
)

// ProcFS 是 /proc 的读取入口。
type ProcFS struct {
	Root   string // 如 /proc
	UserDB string // 用户库（/etc/passwd 格式）路径；空则不解析用户名（只给 uid 数字）
	LE     bool   // 主机字节序（/proc/net 地址）
}

// ListenersFromProc 读取 /proc/net/{tcp,tcp6,udp,udp6}，并经 /proc/<pid>/fd 的 socket:[inode] 反查占用进程。
// 没有权限读取的进程的 fd（非 root 看他人进程）使该套接字 Hidden=true（保留 /proc/net 里的属主 uid）；notes 说明降级。
func (p ProcFS) ListenersFromProc() (socks []Socket, notes []string, err error) {
	type src struct {
		file, proto string
		v6          bool
	}
	var all []ProcNetEntry
	read := 0
	for _, s := range []src{{"net/tcp", "tcp", false}, {"net/tcp6", "tcp", true}, {"net/udp", "udp", false}, {"net/udp6", "udp", true}} {
		b, e := readLimited(filepath.Join(p.Root, s.file))
		if e != nil {
			if !errors.Is(e, fs.ErrNotExist) { // tcp6/udp6 在禁用 IPv6 的内核上不存在
				notes = append(notes, "无法读取 "+filepath.Join(p.Root, s.file)+"："+e.Error())
			}
			continue
		}
		read++
		es, skipped := ParseProcNet(b, s.proto, s.v6, p.LE)
		if skipped > 0 {
			notes = append(notes, s.file+" 有 "+strconv.Itoa(skipped)+" 行无法解析，已跳过")
		}
		all = append(all, es...)
	}
	if read == 0 {
		return nil, notes, errors.New("probe: " + p.Root + "/net/* 均不可读")
	}
	inodes := map[uint64]bool{}
	for _, e := range all {
		inodes[e.Inode] = true
	}
	owners, denied := p.socketOwners(inodes)
	if denied > 0 {
		notes = append(notes, strconv.Itoa(denied)+" 个进程的 fd 目录无权读取（非 root？），其占用者的 pid/进程名不可见；属主取自 /proc/net 的 uid")
	}
	users := p.loadPasswd()
	for _, e := range all {
		s := e.ToSocket()
		if u, ok := users[e.UID]; ok {
			s.Listener.User = u
		} else if e.UID >= 0 {
			s.Listener.User = strconv.Itoa(e.UID)
		}
		if procs := owners[e.Inode]; len(procs) > 0 {
			sort.Slice(procs, func(i, j int) bool { return procs[i].PID < procs[j].PID })
			s.Procs, s.Hidden = procs, false
			s.Listener.PID, s.Listener.Process = procs[0].PID, procs[0].Name
		}
		socks = append(socks, s)
	}
	return socks, notes, nil
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, maxProcFile))
}

// socketOwners 遍历 /proc/<pid>/fd，返回 inode → 占用进程；denied 是读不了 fd 目录的进程数。
func (p ProcFS) socketOwners(want map[uint64]bool) (map[uint64][]ProcRef, int) {
	out := map[uint64][]ProcRef{}
	denied := 0
	es, err := os.ReadDir(p.Root)
	if err != nil {
		return out, 0
	}
	pids := 0
	for _, e := range es {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if pids++; pids > maxPIDs {
			break
		}
		fdDir := filepath.Join(p.Root, e.Name(), "fd")
		fes, err := os.ReadDir(fdDir)
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				denied++
			}
			continue
		}
		name := ""
		seen := map[uint64]bool{}
		for i, fe := range fes {
			if i >= maxFDs {
				break
			}
			tgt, err := os.Readlink(filepath.Join(fdDir, fe.Name()))
			if err != nil || !strings.HasPrefix(tgt, "socket:[") || !strings.HasSuffix(tgt, "]") {
				continue
			}
			ino, err := strconv.ParseUint(tgt[len("socket:["):len(tgt)-1], 10, 64)
			if err != nil || !want[ino] || seen[ino] {
				continue
			}
			seen[ino] = true
			if name == "" {
				name = p.procName(e.Name())
			}
			out[ino] = append(out[ino], ProcRef{PID: pid, Name: name})
		}
	}
	return out, denied
}

func (p ProcFS) procName(pid string) string {
	if b, err := os.ReadFile(filepath.Join(p.Root, pid, "comm")); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	return "?"
}

// Enrich 用 /proc 补全 ss 给出的占用者：属主（/proc/<pid>/status 的 Uid → 用户名）与 systemd 单元（/proc/<pid>/cgroup）。
// 读不到（进程已退出、无权限）的项保持原样。
func (p ProcFS) Enrich(socks []Socket) {
	users := p.loadPasswd()
	for i := range socks {
		l := &socks[i].Listener
		if l.PID <= 0 {
			continue
		}
		pid := strconv.Itoa(l.PID)
		if l.User == "" {
			if uid, ok := readUID(filepath.Join(p.Root, pid, "status")); ok {
				if n, ok := users[uid]; ok {
					l.User = n
				} else {
					l.User = strconv.Itoa(uid)
				}
			}
		}
		if l.Unit == "" {
			if b, err := os.ReadFile(filepath.Join(p.Root, pid, "cgroup")); err == nil {
				l.Unit = UnitFromCgroup(string(b))
			}
		}
	}
}

func readUID(statusPath string) (int, bool) {
	b, err := os.ReadFile(statusPath)
	if err != nil {
		return 0, false
	}
	return parseStatusUID(b)
}

// parseStatusUID 取 /proc/<pid>/status 的 `Uid:\t<real>\t<effective>\t…` 的有效 uid。
func parseStatusUID(b []byte) (int, bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "Uid:"); ok {
			f := strings.Fields(v)
			if len(f) >= 2 {
				if n, err := strconv.Atoi(f[1]); err == nil {
					return n, true
				}
			}
		}
	}
	return 0, false
}

// UnitFromCgroup 从 /proc/<pid>/cgroup 取 systemd 服务单元名。cgroup v2：`0::/system.slice/nginx.service`；
// v1 有多行（`1:name=systemd:/system.slice/nginx.service`）。取路径中最后一个以 .service 结尾的段；找不到返回空。
func UnitFromCgroup(s string) string {
	var unit string
	for _, line := range strings.Split(s, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 {
			continue
		}
		segs := strings.Split(parts[2], "/")
		for i := len(segs) - 1; i >= 0; i-- {
			if strings.HasSuffix(segs[i], ".service") && len(segs[i]) > len(".service") {
				if unit == "" || parts[0] == "0" {
					unit = segs[i]
				}
				break
			}
		}
	}
	return unit
}

func (p ProcFS) loadPasswd() map[int]string {
	m := map[int]string{}
	if p.UserDB == "" {
		return m
	}
	b, err := os.ReadFile(p.UserDB)
	if err != nil {
		return m
	}
	return ParsePasswd(b)
}

// ParsePasswd 解析 /etc/passwd（name:x:uid:gid:…）；同 uid 取先出现者（与 getpwuid 一致）。
func ParsePasswd(b []byte) map[int]string {
	m := map[int]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ":")
		if len(f) < 3 || strings.HasPrefix(f[0], "#") || f[0] == "" {
			continue
		}
		uid, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		if _, dup := m[uid]; !dup {
			m[uid] = f[0]
		}
	}
	return m
}
