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
		{"err-not-implemented", []string{"update", "server", "--json", "--dry-run"}, []hopt{asRoot()}, 1, ""},
		{"err-install-no-keys", []string{"install", "server", "--json", "--dry-run"}, []hopt{asRoot()}, 5, ""}, // 本构建未内置发行公钥：安装/预检失败关闭
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

// 解析失败时 envelope 行为须与 pflag 的实际解析一致：--json 的各种合法/非法写法、出错点之后的 --json、
// 与其他标志混排、`--` 之后、被其他标志当作取值的 --json。
func TestJSONEnvelopeOnParseFailureMatchesPflag(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantJSON bool
	}{
		{"--json 在出错点之前", []string{"list", "--json", "--bogus"}, true},
		{"--json 在出错点之后", []string{"list", "--bogus", "--json"}, true},
		{"--json=true", []string{"list", "--bogus", "--json=true"}, true},
		{"--json=1", []string{"list", "--bogus", "--json=1"}, true},
		{"--json=TRUE", []string{"list", "--bogus", "--json=TRUE"}, true},
		{"--json=True", []string{"list", "--bogus", "--json=True"}, true},
		{"--json=t", []string{"list", "--bogus", "--json=t"}, true},
		{"--json=T", []string{"list", "--bogus", "--json=T"}, true},
		{"--json=false 不是 json", []string{"list", "--bogus", "--json=false"}, false},
		{"--json=0 不是 json", []string{"list", "--bogus", "--json=0"}, false},
		{"--json=maybe 非法 bool 不是 json", []string{"list", "--bogus", "--json=maybe"}, false},
		{"--json=yes 非法 bool 不是 json", []string{"list", "--bogus", "--json=yes"}, false},
		{"后者覆盖前者（假覆盖真）", []string{"list", "--bogus", "--json", "--json=false"}, false},
		{"后者覆盖前者（真覆盖假）", []string{"list", "--bogus", "--json=false", "--json"}, true},
		{"-- 之后的 --json 是位置参数", []string{"list", "--bogus", "--", "--json"}, false},
		{"-- 之前的 --json 生效", []string{"list", "--json", "--bogus", "--", "x"}, true},
		{"--config 的取值是 --json 字样", []string{"list", "--bogus", "--config", "--json"}, false},
		{"--config=--json", []string{"list", "--bogus", "--config=--json"}, false},
		{"与短标志混排", []string{"-y", "list", "--bogus", "--no-color", "--json"}, true},
		{"未知命令后的 --json", []string{"frobnicate", "--json"}, true},
		{"未知命令且 --json=TRUE", []string{"frobnicate", "--json=TRUE"}, true},
		{"取值缺失（--log-level 缺参）后的 --json 不可达", []string{"list", "--json", "--log-level"}, true},
		{"子命令本地标志取值是 --json 字样", []string{"install", "server", "--bogus", "--channel", "--json"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			code := h.run(c.args...)
			if code != 2 {
				t.Fatalf("退出码 %d（应为 2）stdout=%q stderr=%q", code, h.stdout.String(), h.stderr.String())
			}
			if !c.wantJSON {
				if h.stdout.Len() != 0 {
					t.Fatalf("非 --json 时 stdout 应为空: %q", h.stdout.String())
				}
				if !strings.Contains(h.stderr.String(), "退出码 2") {
					t.Errorf("应输出人类错误: %q", h.stderr.String())
				}
				return
			}
			env := h.envelope()
			if env["ok"] != false || env["exit_code"] != 2.0 {
				t.Errorf("envelope: %v", env)
			}
			if h.stderr.Len() != 0 && strings.Contains(h.stderr.String(), "发生了什么") {
				t.Errorf("--json 下不应再输出人类错误: %q", h.stderr.String())
			}
		})
	}
}
