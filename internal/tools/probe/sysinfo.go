package probe

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// ParseMeminfo 解析 /proc/meminfo，返回 MiB。MemAvailable 缺失（内核 < 3.14）时回退为 MemFree+Buffers+Cached（并返回 approx=true）。
func ParseMeminfo(b []byte) (totalMB, availMB int64, approx, ok bool) {
	kv := map[string]int64{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil || n < 0 {
			continue
		}
		if len(f) > 1 {
			switch strings.ToLower(f[1]) {
			case "kb":
			case "mb":
				n *= 1024
			case "gb":
				n *= 1024 * 1024
			case "b":
				n /= 1024
			}
		}
		kv[k] = n
	}
	total, hasT := kv["MemTotal"]
	if !hasT {
		return 0, 0, false, false
	}
	avail, hasA := kv["MemAvailable"]
	if !hasA {
		avail = kv["MemFree"] + kv["Buffers"] + kv["Cached"]
		approx = true
	}
	return total / 1024, avail / 1024, approx, true
}

// MountEntry 是 /proc/self/mountinfo 的一行（只留需要的字段）。
type MountEntry struct {
	Mount    string
	FSType   string
	ReadOnly bool
}

// pseudoFS 是不代表磁盘空间的伪文件系统，不进入 Disks。
var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "cgroup": true, "cgroup2": true, "securityfs": true, "debugfs": true,
	"tracefs": true, "pstore": true, "bpf": true, "mqueue": true, "hugetlbfs": true, "configfs": true, "fusectl": true, "autofs": true,
	"binfmt_misc": true, "rpc_pipefs": true, "nsfs": true, "selinuxfs": true, "efivarfs": true, "fuse.gvfsd-fuse": true, "fuse.portal": true,
	"fuse.lxcfs": true, "squashfs": true, "ramfs": true,
}

// ParseMountinfo 解析 /proc/self/mountinfo：
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
//
// 挂载点中的空格/制表/换行/反斜杠按 \040 \011 \012 \134 八进制转义；可选字段个数不定，以单独的 "-" 为界。伪文件系统被过滤；同一挂载点只留最后一条（后挂载覆盖前者）。
func ParseMountinfo(b []byte) (mounts []MountEntry, skipped int) {
	idx := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		dash := -1
		for i := 6; i < len(f); i++ {
			if f[i] == "-" {
				dash = i
				break
			}
		}
		if len(f) < 10 || dash < 0 || dash+3 >= len(f) {
			skipped++
			continue
		}
		fstype := f[dash+1]
		if pseudoFS[fstype] {
			continue
		}
		mp := unescapeMount(f[4])
		ro := false
		for _, o := range strings.Split(f[5], ",") {
			if o == "ro" {
				ro = true
			}
		}
		for _, o := range strings.Split(f[dash+3], ",") { // super options（含 ro/rw）
			if o == "ro" {
				ro = true
			}
		}
		e := MountEntry{Mount: mp, FSType: fstype, ReadOnly: ro}
		if i, ok := idx[mp]; ok {
			mounts[i] = e
			continue
		}
		idx[mp] = len(mounts)
		mounts = append(mounts, e)
	}
	return mounts, skipped
}

func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				sb.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}
