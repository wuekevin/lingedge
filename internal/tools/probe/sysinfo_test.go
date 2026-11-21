package probe

import (
	"reflect"
	"testing"
)

func TestParseMeminfo(t *testing.T) {
	total, avail, approx, ok := ParseMeminfo(fixture(t, "sys/meminfo"))
	if !ok || approx || total != 8040244/1024 || avail != 5103920/1024 {
		t.Errorf("meminfo: %d %d approx=%v ok=%v", total, avail, approx, ok)
	}
	total, avail, approx, ok = ParseMeminfo(fixture(t, "sys/meminfo-old-kernel"))
	if !ok || !approx || total != 1017288/1024 || avail != (102400+51200+204800)/1024 {
		t.Errorf("旧内核 meminfo: %d %d approx=%v ok=%v", total, avail, approx, ok)
	}
	for _, bad := range []string{"", "garbage", "MemFree: 1 kB\n", "MemTotal: abc kB\n"} {
		if _, _, _, ok := ParseMeminfo([]byte(bad)); ok {
			t.Errorf("%q 应失败", bad)
		}
	}
	if total, _, _, ok := ParseMeminfo([]byte("MemTotal: 2 MB\nMemAvailable: 1 MB\n")); !ok || total != 2 {
		t.Errorf("单位换算: %d", total)
	}
}

func TestParseMountinfo(t *testing.T) {
	ms, skipped := ParseMountinfo(fixture(t, "sys/mountinfo"))
	if skipped != 0 {
		t.Errorf("skipped=%d", skipped)
	}
	got := map[string]MountEntry{}
	for _, m := range ms {
		got[m.Mount] = m
	}
	// 伪文件系统（proc/sysfs/devtmpfs/devpts/cgroup2/securityfs/squashfs）被过滤
	want := map[string]MountEntry{
		"/":                             {"/", "ext4", false},
		"/run":                          {"/run", "tmpfs", false},
		"/dev/shm":                      {"/dev/shm", "tmpfs", false},
		"/boot":                         {"/boot", "ext4", false},
		"/boot/efi":                     {"/boot/efi", "vfat", false},
		"/www":                          {"/www", "xfs", false},
		"/mnt/my disk":                  {"/mnt/my disk", "ext4", true}, // \040 → 空格；ro
		"/var/lib/overlayfs/abc/merged": {"/var/lib/overlayfs/abc/merged", "overlay", false},
		"/data":                         {"/data", "nfs4", false}, // 无可选字段（直接是 "-"）
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mounts:\n got %+v\nwant %+v", got, want)
	}
	// 异常行不 panic
	_, sk := ParseMountinfo([]byte("garbage\n1 2 3\n36 35 98:0 / /x rw - ext4\n\n"))
	if sk != 3 {
		t.Errorf("skipped=%d", sk)
	}
	// ro 可以只出现在挂载选项（绑定挂载只读）或只出现在超级块选项
	ms, _ = ParseMountinfo([]byte("1 0 8:1 / /a ro,relatime - ext4 /dev/a rw\n2 0 8:2 / /b rw,relatime - ext4 /dev/b ro\n3 0 8:3 / /c rw - ext4 /dev/c rw\n"))
	if len(ms) != 3 || !ms[0].ReadOnly || !ms[1].ReadOnly || ms[2].ReadOnly {
		t.Errorf("ro 判定: %+v", ms)
	}
	// 同一挂载点后者覆盖前者
	ms, _ = ParseMountinfo([]byte("1 0 8:1 / /m rw - ext4 /dev/a rw\n2 0 8:2 / /m ro - xfs /dev/b ro\n"))
	if len(ms) != 1 || ms[0].FSType != "xfs" || !ms[0].ReadOnly {
		t.Errorf("覆盖: %+v", ms)
	}
	if unescapeMount(`/a\040b\134c\011`) != "/a b\\c\t" || unescapeMount(`/x\04`) != `/x\04` {
		t.Errorf("unescape: %q", unescapeMount(`/a\040b\134c\011`))
	}
}

func TestParseSystemctlUnits(t *testing.T) {
	svcs := ParseSystemctlUnits(fixture(t, "systemctl/list-units.txt"))
	type k struct{ name, unit, active, role string }
	var got []k
	for _, s := range svcs {
		got = append(got, k{s.Name, s.Unit, s.Active, s.Role})
	}
	want := []k{
		{"apache2", "apache2.service", "failed", "web"}, // ● 前缀（未带 --plain 的失败标记）被剥离
		{"firewalld", "firewalld.service", "inactive", "firewall"},
		{"mysqld", "mysql.service", "active", "db"},
		{"named", "named.service", "inactive", "dns"}, // bind9.service 是 not-found，被忽略；named 取已加载的
		{"nginx", "nginx.service", "active", "web"},
		{"redis", "redis-server.service", "active", "cache"},
		{"systemd-resolved", "systemd-resolved.service", "active", "resolver"},
		{"ufw", "ufw.service", "active", "firewall"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("services:\n got %+v\nwant %+v", got, want)
	}
	if len(ParseSystemctlUnits([]byte("garbage\n\nfoo.service loaded active running x\n"))) != 0 {
		t.Error("未识别的服务不应输出")
	}
	// 同名多单元：active 优先
	s2 := ParseSystemctlUnits([]byte("named.service loaded active running a\nnamed-chroot.service loaded inactive dead b\nbind9.service loaded inactive dead c\n"))
	if len(s2) != 1 || s2[0].Unit != "named.service" || s2[0].Active != "active" {
		t.Errorf("active 优先: %+v", s2)
	}
}
