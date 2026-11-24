package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

const testRunID = "01JD5Y3C8Q5G7M0V1N2P3R4S5T"

// harness 构造真实的命令运行环境：真实临时目录里的 state.json/日志、缓冲区标准流、固定时钟与 run_id。
// 命令树、校验、输出与日志都是生产代码；只有「终端是否为 TTY」「有效 UID」「主机名」「时钟」这些进程环境量由测试决定。
type harness struct {
	t      *testing.T
	env    Env
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	dir    string
}

type hopt func(*harness)

func asRoot() hopt            { return func(h *harness) { h.env.Geteuid = func() int { return 0 } } }
func withTTY() hopt           { return func(h *harness) { h.env.StdinTTY, h.env.StderrTTY = true, true } }
func withStdin(s string) hopt { return func(h *harness) { h.env.Stdin = strings.NewReader(s) } }
func withEnv(kv map[string]string) hopt {
	return func(h *harness) { h.env.Getenv = func(k string) string { return kv[k] } }
}
func withHostname(n string) hopt {
	return func(h *harness) { h.env.Hostname = func() (string, error) { return n, nil } }
}
func withState(s StateSource) hopt { return func(h *harness) { h.env.State = s } }

// noSystemd 让 systemctl 封装认定本机不是 systemd 启动的系统：与运行测试的宿主机无关（宿主机的 systemd 里没有这些测试组件）。
func noSystemd() hopt {
	return func(h *harness) { h.env.Install.Booted = func() bool { return false } }
}

func newHarness(t *testing.T, opts ...hopt) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, dir: dir}
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	calls := 0
	var clockMu sync.Mutex // 进度消费者 goroutine（Story 1.33）与命令主流程会并发读时钟
	h.env = Env{
		Stdin: strings.NewReader(""), Stdout: h.stdout, Stderr: h.stderr,
		Getenv:   func(string) string { return "" },
		Geteuid:  func() int { return 1000 },
		Hostname: func() (string, error) { return "edge-host-1", nil },
		Now: func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			calls++
			return t0.Add(time.Duration(calls-1) * 25 * time.Millisecond)
		},
		NewRunID:       func(time.Time) string { return testRunID },
		StatePath:      filepath.Join(dir, "tools", "state.json"),
		DefaultLogFile: "", // 缺省不写日志；需要日志的用例显式传 --log-file
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

func (h *harness) run(args ...string) int {
	h.t.Helper()
	return h.runCtx(context.Background(), args...)
}

func (h *harness) runCtx(ctx context.Context, args ...string) int {
	h.t.Helper()
	h.stdout.Reset()
	h.stderr.Reset()
	return Execute(ctx, h.env, args)
}

// envelope 解析 stdout：必须恰好是一个 JSON 文档（AC3：stdout 仅一个 envelope）。
func (h *harness) envelope() map[string]any {
	h.t.Helper()
	dec := json.NewDecoder(bytes.NewReader(h.stdout.Bytes()))
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		h.t.Fatalf("stdout 不是 JSON: %v\n%q", err, h.stdout.String())
	}
	if dec.More() {
		h.t.Fatalf("stdout 含多个 JSON 文档: %q", h.stdout.String())
	}
	if !strings.HasSuffix(h.stdout.String(), "\n") || strings.Count(h.stdout.String(), "\n") != 1 {
		h.t.Fatalf("envelope 应为单行加换行: %q", h.stdout.String())
	}
	return m
}

func readSchema(t *testing.T, name string) *openapi3.Schema {
	t.Helper()
	b, err := SchemaFS.ReadFile("schema/" + name)
	if err != nil {
		t.Fatalf("读取 schema %s: %v", name, err)
	}
	var s openapi3.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("解析 schema %s: %v", name, err)
	}
	return &s
}

// validateAgainst 用 schema 文件校验一个 JSON 值。
func validateAgainst(t *testing.T, schemaFile string, v any) error {
	t.Helper()
	return readSchema(t, schemaFile).VisitJSON(v)
}

func decodeAny(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("JSON 无效: %v\n%s", err, b)
	}
	return v
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func errCode(m map[string]any) string {
	errs, _ := m["errors"].([]any)
	if len(errs) == 0 {
		return ""
	}
	e, _ := errs[0].(map[string]any)
	c, _ := e["code"].(string)
	return c
}
