//go:build darwin

package probe

import "testing"

// 开发机（macOS）上的真实采集：内存总量与磁盘来自系统调用；可用内存明确降级为 0。
func TestDarwinRealCollection(t *testing.T) {
	total, avail, notes, err := osMemory()
	if err != nil || total < 512 || avail != 0 || len(notes) == 0 {
		t.Fatalf("memory: %d %d %v %v", total, avail, notes, err)
	}
	ds, _, err := osDisks()
	if err != nil || len(ds) == 0 {
		t.Fatalf("disks: %v %v", ds, err)
	}
	hasRoot := false
	for _, d := range ds {
		if d.Mount == "/" {
			hasRoot = true
			if d.FreeMB <= 0 || d.TotalMB < d.FreeMB {
				t.Errorf("根分区: %+v", d)
			}
		}
	}
	if !hasRoot {
		t.Errorf("缺少 /: %+v", ds)
	}
}
