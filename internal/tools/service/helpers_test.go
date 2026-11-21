package service

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// iniEntry 是单元文件中的一条指令。
type iniEntry struct{ Section, Key, Value string }

// parseINI 是测试用的最小 systemd 单元解析器：只认 [Section]、Key=Value、# 注释与空行；其他任何行（含续行 `\`）都使测试失败。
// 属性测试靠它解析「渲染结果」，而不是拿 golden 文本自证。
func parseINI(t testing.TB, content string) []iniEntry {
	t.Helper()
	var out []iniEntry
	sec := ""
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		ln := sc.Text()
		switch {
		case strings.TrimSpace(ln) == "", strings.HasPrefix(ln, "#"):
		case strings.HasPrefix(ln, "[") && strings.HasSuffix(ln, "]"):
			sec = ln[1 : len(ln)-1]
		default:
			k, v, ok := strings.Cut(ln, "=")
			if !ok || k == "" || strings.ContainsAny(k, " \t") || sec == "" {
				t.Fatalf("不是合法的单元行（含注入迹象？）：%q", ln)
			}
			out = append(out, iniEntry{sec, k, v})
		}
	}
	return out
}

func get(es []iniEntry, sec, key string) []string {
	var out []string
	for _, e := range es {
		if e.Section == sec && e.Key == key {
			out = append(out, e.Value)
		}
	}
	return out
}

func has(es []iniEntry, sec, key string) bool { return len(get(es, sec, key)) > 0 }

func one(t testing.TB, es []iniEntry, sec, key string) string {
	t.Helper()
	v := get(es, sec, key)
	if len(v) != 1 {
		t.Fatalf("[%s] %s 应恰有 1 条，得到 %d 条：%q", sec, key, len(v), v)
	}
	return v[0]
}

func mustRender(t testing.TB, u UnitName) Unit {
	t.Helper()
	switch u {
	case UnitServer:
		r, err := RenderServer(DefaultServerParams())
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r, err := RenderNode(u, DefaultNodeParams())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func readFile(t testing.TB, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func goldenPath(name string) string { return filepath.Join("testdata", "golden", name) }
