package registry

import (
	"runtime"
	"testing"
)

func TestParseProcStat(t *testing.T) {
	// 真实 /proc/<pid>/stat 的形态；comm 含空格与括号。
	line := "1234 (my (weird) proc) S 1 1234 1234 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000000 200 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0"
	if n, ok := parseProcStat(line); !ok || n != 987654 {
		t.Fatalf("starttime=%d ok=%v", n, ok)
	}
	for _, bad := range []string{"", "1234", "1 (a) S 1 2", "1 (a"} {
		if _, ok := parseProcStat(bad); ok {
			t.Errorf("%q 应解析失败", bad)
		}
	}
}

func TestFSKindOnLocalTemp(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("无文件系统类型识别")
	}
	fi, err := FSKind(t.TempDir())
	if err != nil || fi.Type == "" {
		t.Fatalf("%+v %v", fi, err)
	}
	if fi.Network {
		t.Fatalf("本机临时目录不应被判为网络文件系统: %+v", fi)
	}
	if _, err := FSKind("/nonexistent-dir-xyz"); err == nil {
		t.Fatal("不存在的路径应报错")
	}
}

func TestProcStartTicksSelf(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("等待环境验证（ENV-LNX）：/proc 仅 Linux；解析逻辑由 TestParseProcStat 覆盖")
	}
	if procStartTicks(1) == 0 && procStartTicks(selfPID()) == 0 {
		t.Fatal("读不到自身的 starttime")
	}
}
