package cli

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

var update = flag.Bool("update", false, "重写 testdata/golden 快照")

// AC3：--json 时 stdout 仅一个 envelope，真实命令输出同时满足 envelope schema 与该命令的 data schema，并与快照逐字节一致。
func TestJSONSnapshotsAndSchemas(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		opts       []hopt
		wantExit   int
		dataSchema string // 空 = 失败，data 为 {}
	}{
		{"version", []string{"version", "--json"}, nil, 0, "data.version.schema.json"},
		{"root-version", []string{"--version", "--json"}, nil, 0, "data.root-version.schema.json"},
		{"list-empty", []string{"list", "--json"}, nil, 0, "data.list.schema.json"},
		{"status-empty", []string{"status", "--json"}, nil, 0, "data.status.schema.json"},
		{"exit-codes", []string{"--help", "exit-codes", "--json"}, nil, 0, "data.exit-codes.schema.json"},
		{"help-root", []string{"--json"}, nil, 0, "data.help.schema.json"},
		{"err-usage", []string{"install", "nope", "--json"}, nil, 2, ""},
		{"err-root", []string{"install", "server", "--json"}, nil, 3, ""},
		{"err-builtin", []string{"install", "scheduler", "--json"}, nil, 8, ""},
		{"err-confirm", []string{"uninstall", "server", "--json"}, []hopt{asRoot()}, 9, ""},
		{"err-not-implemented", []string{"install", "server", "--json", "--dry-run"}, []hopt{asRoot()}, 1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, c.opts...)
			if code := h.run(c.args...); code != c.wantExit {
				t.Fatalf("退出码 %d，期望 %d\nstdout=%s\nstderr=%s", code, c.wantExit, h.stdout, h.stderr)
			}
			env := h.envelope()
			if err := validateAgainst(t, "envelope.schema.json", env); err != nil {
				t.Fatalf("envelope 不符合 schema: %v", err)
			}
			if c.dataSchema != "" {
				if err := validateAgainst(t, c.dataSchema, env["data"]); err != nil {
					t.Fatalf("data 不符合 %s: %v", c.dataSchema, err)
				}
			} else if len(env["data"].(map[string]any)) != 0 || len(env["errors"].([]any)) != 1 {
				t.Errorf("失败的 envelope 应 data={} 且恰一个错误: %v", env)
			}
			// 快照：版本信息随构建变化的字段在快照里归一。
			got := normalizeSnapshot(h.stdout.String())
			golden := filepath.Join("testdata", "golden", c.name+".json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("缺少快照 %s（go test -run TestJSONSnapshotsAndSchemas -update 生成）: %v", golden, err)
			}
			if got != string(want) {
				t.Errorf("输出与快照 %s 不一致\n got: %s\nwant: %s", golden, got, want)
			}
		})
	}
}

// normalizeSnapshot 归一与构建/平台相关的字段（Go 版本、OS/ARCH、commit、help 文本里的 Cobra 排版不稳定部分不进快照）。
func normalizeSnapshot(s string) string {
	for _, p := range [][2]string{{`"go":"`, `"`}, {`"os":"`, `"`}, {`"arch":"`, `"`}, {`"commit":"`, `"`}} {
		s = replaceBetween(s, p[0], p[1], "X")
	}
	return s
}

func replaceBetween(s, open, closeS, with string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return s
	}
	j := strings.Index(s[i+len(open):], closeS)
	if j < 0 {
		return s
	}
	return s[:i+len(open)] + with + s[i+len(open)+j:]
}

// schema 目录与命令树互相对账：每个已交付（非 Hidden）命令都有 data schema；schema 文件都有归属。
func TestEverySchemaHasOwnerAndEveryVisibleCommandHasSchema(t *testing.T) {
	r := &Run{env: newHarness(t).env.withDefaults(), ctx: t.Context()}
	root := buildRoot(r)
	owners := map[string]bool{"envelope.schema.json": true, "data.help.schema.json": true, "data.exit-codes.schema.json": true, "data.root-version.schema.json": true}
	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		for _, sub := range c.Commands() {
			if sub.Hidden || sub.Name() == "help" {
				continue
			}
			p := strings.TrimSpace(path + " " + sub.Name())
			file := "data." + strings.ReplaceAll(p, " ", "-") + ".schema.json"
			if len(sub.Commands()) == 0 {
				if _, err := SchemaFS.ReadFile("schema/" + file); err != nil {
					t.Errorf("已交付命令 %q 缺少 data schema %s", p, file)
				}
				owners[file] = true
			}
			walk(sub, p)
		}
	}
	walk(root, "")
	ents, _ := SchemaFS.ReadDir("schema")
	for _, e := range ents {
		if !owners[e.Name()] {
			t.Errorf("schema 文件 %s 没有对应的已交付命令", e.Name())
		}
	}
}

// schema 本身要能拒绝坏输出（避免「什么都放行」的 schema）。
func TestSchemasRejectBadDocuments(t *testing.T) {
	good := decodeAny(t, mustRead(t, "testdata/examples/list.nonempty.json"))
	if err := validateAgainst(t, "data.list.schema.json", good); err != nil {
		t.Fatalf("合法示例被拒绝: %v", err)
	}
	bad := []struct{ schema, doc string }{
		{"data.list.schema.json", `{"components":[{"name":"node"}]}`},              // 缺字段
		{"data.list.schema.json", `{"components":[],"extra":1}`},                   // 多余字段
		{"data.version.schema.json", `{"tools":{"version":"1"},"components":[]}`},  // tools 缺字段
		{"data.status.schema.json", `{"components":[{"name":"a","version":"1"}]}`}, // 缺 state
		{"envelope.schema.json", `{"schema":"lingedge.cli/v2"}`},                   // 版本不符
		{"data.help.schema.json", `{"help":""}`},                                   // 空帮助
	}
	for _, b := range bad {
		if err := validateAgainst(t, b.schema, decodeAny(t, []byte(b.doc))); err == nil {
			t.Errorf("%s 应拒绝 %s", b.schema, b.doc)
		}
	}
	env := map[string]any{"schema": SchemaID, "ok": true, "command": "x", "run_id": testRunID, "exit_code": 0.0, "exit_name": "OK",
		"started_at": "2026-11-21T12:00:00.000Z", "duration_ms": 1.0, "dry_run": false, "data": map[string]any{}, "warnings": []any{}, "errors": []any{}}
	if err := validateAgainst(t, "envelope.schema.json", env); err != nil {
		t.Fatalf("合法 envelope 被拒绝: %v", err)
	}
	env["exit_code"] = 77.0
	if validateAgainst(t, "envelope.schema.json", env) == nil {
		t.Error("表外退出码应被拒绝")
	}
}

// 解析失败（未知标志）与帮助：--json 下 stdout 同样只有一个 envelope。
func TestJSONOnlyEnvelopeEvenOnParseFailure(t *testing.T) {
	h := newHarness(t)
	if code := h.run("list", "--bogus", "--json"); code != 2 {
		t.Fatalf("退出码 %d", code)
	}
	env := h.envelope()
	if env["command"] != "list" || errCode(env) != "usage.invalid_flag" {
		t.Errorf("envelope: %v", env)
	}
	if bytes.Contains(h.stdout.Bytes(), []byte("Usage")) {
		t.Error("--json 下 stdout 不得含 Cobra 用法文本")
	}
}
