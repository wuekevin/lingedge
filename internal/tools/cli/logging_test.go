package cli

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRedactArgs(t *testing.T) {
	cases := []struct{ in, want []string }{
		{[]string{"node", "enroll", "--token", "lenr1.AAAA"}, []string{"node", "enroll", "--token", "***"}},
		{[]string{"node", "enroll", "--token=lenr1.AAAA", "--server", "https://x"}, []string{"node", "enroll", "--token=***", "--server", "https://x"}},
		{[]string{"node", "enroll", "--token-file", "/root/t"}, []string{"node", "enroll", "--token-file", "/root/t"}},
		{[]string{"install", "server", "--set", "db.password=hunter2", "--set", "listen=:80"}, []string{"install", "server", "--set", "db.password=***", "--set", "listen=:80"}},
		{[]string{"install", "server", "--set=api_key=abc"}, []string{"install", "server", "--set=api_key=***"}},
		{[]string{"x", "--admin-password", "p"}, []string{"x", "--admin-password", "***"}},
		{[]string{"x", "--", "--token", "keep"}, []string{"x", "--", "--token", "keep"}}, // `--` 之后是位置参数
		{[]string{"x", "--token"}, []string{"x", "--token"}},                             // 缺值不越界
	}
	for _, c := range cases {
		if got := redactArgs(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("redactArgs(%v) = %v，期望 %v", c.in, got, c.want)
		}
	}
}

// AC6：日志是 JSON Lines（ts/level/run_id/msg/fields），--token 记为 ***；敏感值不进日志、stdout、stderr。
func TestLogFileJSONLRedacted(t *testing.T) {
	h := newHarness(t, asRoot())
	logf := filepath.Join(h.dir, "logs", "tools.log")
	// 注入值运行期随机生成（G-6：测试源码里不放固定口令字面量）。
	tokVal, pwVal := "lenr1."+randVal(t, "t"), randVal(t, "pw-")
	pwKey := "database." + "pass" + "word"
	code := h.run("node", "enroll", "--token", tokVal, "--server", "https://panel.example.com",
		"--set", "x", "--log-file", logf, "--log-level", "debug", "--json")
	// --set 无效 → 2；换成合法组合再来一遍
	if code != 2 {
		t.Fatalf("退出码 %d", code)
	}
	h.run("install", "server", "--set", pwKey+"="+pwVal, "--set", "note=ok", "--log-file", logf, "--json")
	h.run("node", "enroll", "--token="+tokVal, "--server", "https://u:"+pwVal+"@panel.example.com", "--log-file", logf, "--json")
	h.run("node", "enroll", "--token", tokVal, "--log-file", logf)

	raw, err := os.ReadFile(logf)
	if err != nil {
		t.Fatal(err)
	}
	log := string(raw)
	for _, s := range []string{tokVal, pwVal} {
		if strings.Contains(log, s) {
			t.Errorf("日志泄露了 %.10s…\n%s", s, log)
		}
	}
	if !strings.Contains(log, `"--token","***"`) && !strings.Contains(log, `"--token=***"`) {
		t.Errorf("日志应把 --token 记为 ***:\n%s", log)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimRight(log, "\n"), "\n") {
		var l map[string]any
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("日志行不是 JSON: %q", line)
		}
		for _, k := range []string{"ts", "level", "run_id", "msg"} {
			if _, ok := l[k]; !ok {
				t.Errorf("日志行缺少 %s: %s", k, line)
			}
		}
		if _, err := time.Parse(time.RFC3339Nano, l["ts"].(string)); err != nil {
			t.Errorf("ts 不是 RFC3339: %v", l["ts"])
		}
		n++
	}
	if n < 4 {
		t.Errorf("日志行数 %d 过少", n)
	}
	st, err := os.Stat(logf)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Errorf("日志文件权限 %v，期望 0640", st.Mode().Perm())
	}
	for name, out := range map[string]string{"stdout": h.stdout.String(), "stderr": h.stderr.String()} {
		for _, s := range []string{tokVal, pwVal} {
			if strings.Contains(out, s) {
				t.Errorf("%s 泄露了敏感值", name)
			}
		}
	}
}

// --log-level 过滤；显式 --log-file 不可写 → 3；缺省日志路径不可写 → 静默禁用而不影响只读命令。
func TestLogLevelAndUnwritable(t *testing.T) {
	h := newHarness(t)
	logf := filepath.Join(h.dir, "warn.log")
	if code := h.run("list", "--log-file", logf, "--log-level", "error"); code != 0 {
		t.Fatalf("退出码 %d", code)
	}
	if b, _ := os.ReadFile(logf); len(b) != 0 {
		t.Errorf("--log-level error 不应记录 info: %s", b)
	}
	blocker := filepath.Join(h.dir, "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := h.run("list", "--json", "--log-file", filepath.Join(blocker, "x.log")); code != 3 {
		t.Errorf("显式 --log-file 不可写应 3，得 %d", code)
	}
	if got := errCode(h.envelope()); got != "log.file_unwritable" {
		t.Errorf("errors.code=%q", got)
	}
	h2 := newHarness(t)
	h2.env.DefaultLogFile = filepath.Join(blocker, "default.log")
	if code := h2.run("list"); code != 0 {
		t.Errorf("缺省日志路径不可写不应让只读命令失败: %d", code)
	}
}

// envelope 对 data/errors/warnings 兜底脱敏（即使将来的命令把机密放进 data 或错误细节）。
func TestEnvelopeSanitizesAllFields(t *testing.T) {
	leakVal := "tok-" + randVal(t, "")
	e := &Envelope{Schema: SchemaID, Command: "x", RunID: testRunID, ExitCode: 1, ExitName: "GENERAL", StartedAt: "2026-11-21T12:00:00.000Z",
		Data:     map[string]any{"token": leakVal, "nested": map[string]any{"password": leakVal}, "note": "password=" + leakVal},
		Warnings: []Warning{{Code: "a.b", Message: "x password=" + leakVal}},
		Errors:   []ErrorItem{{Code: "a.b", Message: "dsn lingedge:" + leakVal + "@tcp(127.0.0.1:3306)/db", Hint: "--token " + leakVal, Details: map[string]any{"api_key": leakVal}}}}
	var sb strings.Builder
	if err := e.write(&sb); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb.String(), leakVal) {
		t.Errorf("envelope 泄露: %s", sb.String())
	}
}

func randVal(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}
