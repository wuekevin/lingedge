package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const defaultBuf = "go run github.com/bufbuild/buf/cmd/buf@v1.75.0" // 与 Makefile 的 BUF_VERSION 一致；make 传入 BUF 时以环境变量为准

// copyProtoModule 把仓库 proto 模块（buf.yaml + .proto，不含生成物）复制到 dst。
func copyProtoModule(t *testing.T, dst string) {
	t.Helper()
	src := filepath.Join(repoRoot(t), "proto")
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if strings.HasSuffix(p, ".pb.go") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func mutateProto(t *testing.T, dir, file, old, new string) {
	t.Helper()
	p := filepath.Join(dir, "lingedge", "node", "v1", file)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), old) {
		t.Fatalf("%s 中找不到 %q", file, old)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBufLintAndBreaking 真实运行 buf（固定版本）：lint 通过；对已发布 .proto 的破坏性变更必须失败，向后兼容新增必须通过（AC3）。
func TestBufLintAndBreaking(t *testing.T) {
	buf := toolCmd("BUF", defaultBuf)
	base := t.TempDir()
	copyProtoModule(t, base)

	if out, err := runTool(t, buf, base, "lint"); err != nil {
		t.Fatalf("真实 proto 应通过 buf lint: %v\n%s", err, out)
	}
	if out, err := runTool(t, buf, base, "build", "-o", os.DevNull); err != nil {
		t.Fatalf("真实 proto 应能编译: %v\n%s", err, out)
	}

	// 先找一个稳定的字段/消息作变异点。
	b, _ := os.ReadFile(filepath.Join(base, "lingedge", "node", "v1", "common.proto"))
	if !strings.Contains(string(b), "message Digest") {
		t.Fatal("common.proto 缺少 message Digest，变异点失效")
	}

	type mutation struct {
		name, file, old, new string
	}
	breaking := []mutation{
		{"删除枚举值", "common.proto", "  ROLE_WORKER = 5;     // 独立 Worker（P3）\n", ""},
		{"枚举值改号", "common.proto", "ROLE_NODE = 1;", "ROLE_NODE = 11;"},
	}
	for _, m := range breaking {
		cur := t.TempDir()
		copyProtoModule(t, cur)
		mutateProto(t, cur, m.file, m.old, m.new)
		out, err := runTool(t, buf, cur, "breaking", ".", "--against", base)
		if err == nil {
			t.Errorf("%s: buf breaking 应失败\n%s", m.name, out)
		}
		// 必须是破坏性检测的结论（规则 ID 形如 FILE_NO_DELETE/ENUM_VALUE_NO_DELETE），而不是语法/编译错误。
		if !strings.Contains(out, "lingedge/node/v1/") || strings.Contains(out, "syntax error") || strings.Contains(out, "unknown type") || strings.Contains(out, "cannot find") || strings.Contains(out, "not defined") {
			t.Errorf("%s: 失败原因应为破坏性变更而非编译错误:\n%s", m.name, out)
		}
		t.Logf("%s => %s", m.name, strings.TrimSpace(out))
	}

	// 删除消息：基线多一个无引用的叶子消息，当前版本没有它（等价于已发布消息被删除）。
	withLeaf := t.TempDir()
	copyProtoModule(t, withLeaf)
	mutateProto(t, withLeaf, "common.proto", "message Digest {", "message LeafInBase {\n  string x = 1;\n}\n\nmessage Digest {")
	if out, err := runTool(t, buf, base, "breaking", ".", "--against", withLeaf); err == nil || !strings.Contains(out, "LeafInBase") {
		t.Errorf("删除已发布消息应被 buf breaking 拒绝: %v\n%s", err, out)
	}

	// 向后兼容：新增 optional 字段到 Digest、新增消息 —— 必须通过。
	cur := t.TempDir()
	copyProtoModule(t, cur)
	mutateProto(t, cur, "common.proto", "message Digest {", "message Digest {\n  string note_added_in_test = 99;")
	if err := os.WriteFile(filepath.Join(cur, "lingedge", "node", "v1", "added.proto"), []byte("syntax = \"proto3\";\npackage lingedge.node.v1;\nmessage AddedInTest {\n  string x = 1;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runTool(t, buf, cur, "breaking", ".", "--against", base); err != nil {
		t.Errorf("向后兼容新增应通过 buf breaking: %v\n%s", err, out)
	}

	// 字段类型/编号变更：找到 Digest 的第一个字段，改编号。
	cur2 := t.TempDir()
	copyProtoModule(t, cur2)
	text := string(b)
	idx := strings.Index(text, "message Digest")
	field := regexpFirstField(text[idx:])
	if field == "" {
		t.Fatal("找不到 Digest 的字段")
	}
	mutateProto(t, cur2, "common.proto", field, strings.Replace(field, "= 1;", "= 7;", 1))
	if out, err := runTool(t, buf, cur2, "breaking", ".", "--against", base); err == nil {
		t.Errorf("字段编号变更应失败\n%s", out)
	}

	// lint 负向：引入违规命名（枚举无 UNSPECIFIED 零值）必须被拒。
	lintBad := t.TempDir()
	copyProtoModule(t, lintBad)
	if err := os.WriteFile(filepath.Join(lintBad, "lingedge", "node", "v1", "bad.proto"), []byte("syntax = \"proto3\";\npackage lingedge.node.v1;\nenum badEnum {\n  FIRST = 0;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runTool(t, buf, lintBad, "lint"); err == nil {
		t.Errorf("违规命名应被 buf lint 拒绝\n%s", out)
	}
}

// regexpFirstField 返回消息体内第一条 `<type> <name> = 1;` 形态的字段行（不含前导空白）。
func regexpFirstField(s string) string {
	for _, line := range strings.Split(s, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasSuffix(strings.Split(l, "//")[0], "= 1;") || strings.Contains(l, "= 1; ") {
			if i := strings.Index(l, "= 1;"); i > 0 {
				return l[:i+4]
			}
		}
	}
	return ""
}

// wrapBuf 返回一个包装 buf 的命令：先真实运行 buf，再按 mode 篡改输出目录（--output 之后的参数）。
func wrapBuf(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "wrap.sh")
	body := `#!/usr/bin/env bash
set -euo pipefail
mode="$1"; shift
"$@"
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--output" ]; then out="$a"; fi
  prev="$a"
done
case "$mode" in
  nonce) for f in $(find "$out" -name '*.pb.go'); do echo "// $RANDOM$RANDOM$RANDOM" >> "$f"; done ;;
  broken) echo "this is not go" >> "$out/lingedge/node/v1/common.pb.go" ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return "bash " + script + " " + mode + " " + strings.Join(toolCmd("BUF", defaultBuf), " ")
}

// TestProtoGenCheckScript 真实运行 scripts/proto-check-gen.sh：真实 proto 通过；非确定性生成、不可编译的生成物、生成失败均必须失败（AC3「可再现」）。
func TestProtoGenCheckScript(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "proto-check-gen.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "proto-check-gen.sh"), script, 0o755); err != nil {
		t.Fatal(err)
	}
	copyProtoModule(t, filepath.Join(root, "proto"))
	run := func(buf string) (string, error) {
		cmd := exec.Command("bash", filepath.Join(root, "scripts", "proto-check-gen.sh"))
		cmd.Env = append(os.Environ(), "BUF="+buf)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	realBuf := strings.Join(toolCmd("BUF", defaultBuf), " ")
	if out, err := run(realBuf); err != nil || !strings.Contains(out, "真实编译、vet 通过") {
		t.Fatalf("真实 proto 应通过: %v\n%s", err, out)
	}
	if out, err := run(wrapBuf(t, "nonce")); err == nil || !strings.Contains(out, "不可再现") {
		t.Fatalf("非确定性生成必须失败: %v\n%s", err, out)
	}
	if out, err := run(wrapBuf(t, "broken")); err == nil || !strings.Contains(out, "编译/vet 失败") {
		t.Fatalf("生成物不可编译必须失败: %v\n%s", err, out)
	}
	// 生成失败：proto 引用了不存在的导入。
	bad := filepath.Join(root, "proto", "lingedge", "node", "v1", "bad.proto")
	if err := os.WriteFile(bad, []byte("syntax = \"proto3\";\npackage lingedge.node.v1;\nimport \"nope/missing.proto\";\nmessage X {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run(realBuf); err == nil {
		t.Fatalf("buf generate 失败必须使脚本失败\n%s", out)
	}
}
