package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/tests/toolsworld"
)

var (
	fuzzWorldOnce sync.Once
	fuzzWorld     *toolsworld.World
)

// 外部可控入口的 fuzz（Plan JSON）：任意 --set 取值经完整的 dry-run 流水线（解析 → 预检 → resolver → Plan 渲染 → envelope）后，
// 输出必须是单个合法的 envelope（schema 校验），成功时 data 必须符合 data.install.schema.json，且不得 panic、不得改动文件系统（dry-run 零副作用）。
func FuzzInstallDryRunSet(f *testing.F) {
	for _, s := range []string{"port.node.tcp.9100=19100@127.0.0.1", "greeting=hi", "port.node.tcp.9100=1", "port.openresty.tcp.80=8080", "k=é", "=", "port.node.tcp.9100=70000"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, set string) {
		fuzzWorldOnce.Do(func() {
			w, err := toolsworld.NewWorld(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			fuzzWorld = w
		})
		w := fuzzWorld
		var out, errb bytes.Buffer
		env := Env{Stdout: &out, Stderr: &errb, Getenv: func(string) string { return "" }, Geteuid: func() int { return 0 },
			Hostname: func() (string, error) { return "h", nil }, Now: func() time.Time { return toolsworld.T0 }, StatePath: w.Layout.StateFile(),
			BuiltinKeys: func() ([]byte, error) { return toolsworld.KeysJSON(), nil }}
		h := &harness{t: t, env: env, stdout: &out, stderr: &errb}
		applyWorld(t, h, w)
		before := dirSnapshot(t, w.Root)
		code := Execute(context.Background(), h.env, []string{"install", "node", "--offline", w.Bundle, "--dry-run", "--json", "--non-interactive", "--set", set})
		if after := dirSnapshot(t, w.Root); after != before {
			t.Fatalf("dry-run 改动了文件系统（--set %q）", set)
		}
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
		if err := dec.Decode(&m); err != nil || dec.More() {
			t.Fatalf("stdout 必须恰是一个 JSON 文档（--set %q）: %v\n%s", set, err, out.String())
		}
		if err := validateAgainst(t, "envelope.schema.json", m); err != nil {
			t.Fatalf("envelope 不符合 schema（--set %q）: %v", set, err)
		}
		if int(m["exit_code"].(float64)) != code {
			t.Fatalf("envelope 与退出码不一致")
		}
		if code == 0 {
			if err := validateAgainst(t, "data.install.schema.json", m["data"]); err != nil {
				t.Fatalf("data 不符合 schema（--set %q）: %v", set, err)
			}
		}
		if strings.Contains(out.String(), "panic") {
			t.Fatalf("输出含 panic")
		}
	})
}
