package migrate

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/config"
)

// 交互输入只在 TTY 可用，且是优先级最低的来源；非 TTY 给出 ErrNoCredentials（退出码 3）。
func TestResolveCredsPriorityAndInteractive(t *testing.T) {
	oldTTY, oldRead := isTTY, readSecretPrompt
	defer func() { isTTY, readSecretPrompt = oldTTY, oldRead }()
	prompts := 0
	readSecretPrompt = func(label string, _ io.Writer) (string, error) { prompts++; return "typed-" + label[:3], nil }
	fc := &FileCreds{}
	fc.Password = secretOf("from-file")

	// 环境变量 ＞ 口令文件 ＞ migrate.yaml ＞ 交互。
	isTTY = func() bool { return true }
	if s, err := resolveCreds(map[string]string{EnvDBPass: "from-env"}, fc, io.Discard); err != nil || s.Reveal() != "from-env" {
		t.Errorf("环境变量应最优先: %v", err)
	}
	pwf := filepath.Join(t.TempDir(), "p")
	if err := os.WriteFile(pwf, []byte("from-pwfile\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if s, err := resolveCreds(map[string]string{EnvDBPassFile: pwf}, fc, io.Discard); err != nil || s.Reveal() != "from-pwfile" {
		t.Errorf("口令文件应优先于 migrate.yaml: %v", err)
	}
	if s, err := resolveCreds(nil, fc, io.Discard); err != nil || s.Reveal() != "from-file" {
		t.Errorf("migrate.yaml 应优先于交互: %v", err)
	}
	if prompts != 0 {
		t.Errorf("有更高优先级来源时不应交互，prompts=%d", prompts)
	}
	if s, err := resolveCreds(nil, nil, io.Discard); err != nil || !strings.HasPrefix(s.Reveal(), "typed-") || prompts != 1 {
		t.Errorf("TTY 且无其他来源应交互: %v prompts=%d", err, prompts)
	}
	// 非 TTY：ErrNoCredentials，且不会交互。
	isTTY = func() bool { return false }
	if _, err := resolveCreds(nil, nil, io.Discard); !errors.Is(err, ErrNoCredentials) || prompts != 1 {
		t.Errorf("非 TTY 应 ErrNoCredentials 且不交互: %v prompts=%d", err, prompts)
	}
	// 互斥。
	if _, err := resolveCreds(map[string]string{EnvDBPass: "a", EnvDBPassFile: pwf}, nil, io.Discard); err == nil {
		t.Error("口令与口令文件应互斥")
	}
	// 交互读到空口令：拒绝。
	isTTY = func() bool { return true }
	readSecretPrompt = func(string, io.Writer) (string, error) { return "", nil }
	if _, err := resolveCreds(nil, nil, io.Discard); err == nil {
		t.Error("空口令应拒绝")
	}
}

func TestGeneratePasswordEntropyAndAlphabet(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		p, err := GeneratePassword()
		if err != nil || len(p) != 32 || strings.ContainsAny(p, "'\"\\ \n+/=") {
			t.Fatalf("生成的口令应为 32 字符 base64url（可安全进入 SQL 字面量）: %q %v", p, err)
		}
		if seen[p] {
			t.Fatal("重复")
		}
		seen[p] = true
	}
}

func TestWriteMigrateYAMLNoOverwriteAndMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "migrate.yaml")
	fc := FileCreds{Host: "h", Port: 3306, Database: "d", User: "lingedge_migrate", Password: secretOf("pw-" + strings.Repeat("x", 8))}
	if err := writeMigrateYAML(p, fc, false); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o400 {
		t.Errorf("应为 0400: %v", st.Mode())
	}
	if err := writeMigrateYAML(p, fc, false); err == nil || !strings.Contains(err.Error(), "拒绝覆盖") {
		t.Errorf("不覆盖模式遇到已存在文件应拒绝: %v", err)
	}
	if err := writeMigrateYAML(p, fc, true); err != nil {
		t.Errorf("覆盖模式: %v", err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("不得残留临时文件: %v", ents)
	}
	if loaded, err := loadMigrateYAML(p, true); err != nil || loaded.Host != "h" || loaded.Password.Reveal() != fc.Password.Reveal() {
		t.Errorf("写出的文件应可被读取校验: %v", err)
	}
}

func secretOf(s string) config.Secret { return config.NewSecret(s) }
