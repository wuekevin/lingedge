package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 真实二进制：进程退出码与 envelope 一致，stdout 在 --json 下只有一个 JSON 文档（管道 = 无 TTY）。
func TestBinaryExitCodesAndEnvelope(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过真实编译")
	}
	bin := filepath.Join(t.TempDir(), "lingedge")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("编译失败: %v\n%s", err, out)
	}
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"version", "--json"}, 0},
		{[]string{"list", "--json", "--log-file", filepath.Join(t.TempDir(), "l.log")}, 0},
		{[]string{"install", "nope", "--json"}, 2},
		{[]string{"install", "scheduler", "--json"}, 8},
		{[]string{"frobnicate", "--json"}, 2},
		{[]string{"--help", "exit-codes", "--json"}, 0},
	}
	if os.Geteuid() != 0 {
		cases = append(cases, struct {
			args []string
			want int
		}{[]string{"install", "server", "--json"}, 3})
	}
	for _, c := range cases {
		cmd := exec.Command(bin, c.args...)
		cmd.Env = append(os.Environ(), "TERM=dumb")
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != c.want {
			t.Errorf("%v 退出码 %d，期望 %d\nstderr=%s", c.args, code, c.want, errb.String())
			continue
		}
		var env map[string]any
		dec := json.NewDecoder(&out)
		if err := dec.Decode(&env); err != nil || dec.More() {
			t.Errorf("%v: stdout 应恰为一个 JSON 文档: %v\n%s", c.args, err, out.String())
			continue
		}
		if int(env["exit_code"].(float64)) != code || env["schema"] != "lingedge.cli/v1" {
			t.Errorf("%v: envelope 与退出码不一致: %v", c.args, env)
		}
	}
	// 无参数（非 TTY）：打印帮助，退出 0，且 stdout 含已交付命令；stderr 仅有一次自动非交互提示。
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "TERM=dumb")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("无参数应退出 0: %v", err)
	}
	if !strings.Contains(out.String(), "version") || strings.Count(errb.String(), "已自动启用 --non-interactive") != 1 {
		t.Errorf("无参数输出不符: out=%q err=%q", out.String(), errb.String())
	}
}
