package toolsinstall

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

func realListeners(t *testing.T, port int, l net.Listener) []resolver.Listener {
	t.Helper()
	a := l.Addr().(*net.TCPAddr)
	return []resolver.Listener{{Proto: "tcp", Addr: a.IP.String(), Port: port, Process: "test-listener", PID: os.Getpid()}}
}

// tree 是整棵临时根的快照（路径 → 类型/权限/内容摘要），排除 Tools 自己的事务簿记（journal 归档、锁、信任库 sequence）、伪 systemd 的 reload 计数、测试控制与 run/ 下的运行期文件。
func tree(t *testing.T, w *toolsworld.World) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(w.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(w.Root, p)
		if rel == "." {
			return nil
		}
		switch {
		case rel == "var/lib/lingedge/tools", strings.HasPrefix(rel, "var/lib/lingedge/tools/"), rel == "fake/reloads", strings.HasPrefix(rel, "run/"), strings.HasPrefix(rel, "ctl/"), rel == "bundle.tar":
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			tg, _ := os.Readlink(p)
			out[rel] = "L " + tg
		case info.IsDir():
			out[rel] = "D " + info.Mode().Perm().String()
		default:
			b, _ := os.ReadFile(p)
			h := sha256.Sum256(b)
			out[rel] = "F " + info.Mode().Perm().String() + " " + hex.EncodeToString(h[:8])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diffTrees(a, b map[string]string) string {
	var lines []string
	for k, v := range a {
		if bv, ok := b[k]; !ok {
			lines = append(lines, "- "+k)
		} else if bv != v {
			lines = append(lines, "~ "+k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			lines = append(lines, "+ "+k)
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
