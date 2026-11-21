package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func sha256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// run 以参数数组执行外部命令（无 shell；G-12），返回合并输出前先区分 stdout/stderr。
func execCmd(dir string, env []string, name string, args ...string) (stdout []byte, err error) {
	var so, se bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		return so.Bytes(), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(se.String()))
	}
	return so.Bytes(), nil
}

// goRun 是对 go 命令的封装（实际执行 `go <args>`）。
func goRun(dir string, env []string, args ...string) ([]byte, error) {
	return execCmd(dir, env, "go", args...)
}

func gitRun(dir string, args ...string) (string, error) {
	out, err := execCmd(dir, nil, "git", args...)
	return strings.TrimSpace(string(out)), err
}

func writeJSON(path string, v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return b, os.WriteFile(path, b, 0o644)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
