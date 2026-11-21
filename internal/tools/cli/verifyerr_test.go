package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

// 仅测试：确定性测试密钥（只存在于 _test.go）。
func cliTestKey(label string) ed25519.PrivateKey {
	s := sha256.Sum256([]byte("LingEdge cli unit test key v1 - TEST ONLY: " + label))
	return ed25519.NewKeyFromSeed(s[:])
}

type cliKeys struct {
	active, revoked, stranger ed25519.PrivateKey
	trust                     *manifest.TrustSet
}

func newCLIKeys(t *testing.T) cliKeys {
	t.Helper()
	k := cliKeys{active: cliTestKey("active"), revoked: cliTestKey("revoked"), stranger: cliTestKey("stranger")}
	ent := func(id string, p ed25519.PrivateKey) map[string]any {
		return map[string]any{"key_id": id, "alg": "ed25519", "status": "active", "public_key": base64.StdEncoding.EncodeToString(p.Public().(ed25519.PublicKey))}
	}
	doc, _ := json.Marshal(map[string]any{"schema_version": 1, "keys": []any{ent("rel-cli-a", k.active), ent("rel-cli-r", k.revoked)}, "revoked_key_ids": []string{"rel-cli-r"}})
	kf, err := manifest.ParseKeysFile(doc)
	if err != nil {
		t.Fatal(err)
	}
	k.trust, err = manifest.NewTrustSet(kf)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func cliManifest(seq int, validUntil string) []byte {
	b, _ := json.Marshal(map[string]any{
		"schema_version": 1, "channel": "stable", "sequence": seq, "generated_at": "2026-11-01T00:00:00Z", "valid_until": validUntil,
		"revoked_versions": []any{map[string]any{"component": "node", "version": "0.0.9", "reason": "x"}}, "components": map[string]any{},
	})
	return b
}

func cliSig(id string, p ed25519.PrivateKey, raw []byte) []byte {
	s := ed25519.Sign(p, append([]byte(manifest.ManifestDomain), raw...))
	b, _ := json.Marshal(map[string]string{"key_id": id, "alg": "ed25519", "sig": base64.StdEncoding.EncodeToString(s)})
	return b
}

type fixedSeq int64

func (f fixedSeq) Max(string) (int64, bool, error) { return int64(f), f > 0, nil }
func (f fixedSeq) Advance(string, int64) error     { return nil }

// finishWith 让真实的 Run.finish 处理一个命令返回的错误，并解析 --json 的 envelope。
func finishWith(t *testing.T, err error) (int, map[string]any) {
	t.Helper()
	var out, errb bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errb, Stdin: bytes.NewReader(nil), Now: func() time.Time { return time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) },
		NewRunID: func(time.Time) string { return testRunID }, Getenv: func(string) string { return "" }, Geteuid: func() int { return 0 }}
	env = env.withDefaults()
	r := &Run{env: env, ctx: t.Context(), started: env.Now(), runID: testRunID, jsonPre: true}
	code := r.finish(&cobra.Command{Use: "install"}, err)
	var m map[string]any
	if e := json.Unmarshal(out.Bytes(), &m); e != nil {
		t.Fatalf("stdout 不是 envelope: %v %q", e, out.String())
	}
	if e := validateAgainst(t, "envelope.schema.json", m); e != nil {
		t.Fatalf("envelope 不符合 schema: %v", e)
	}
	return code, m
}

// 退出码 5 的真实生产者：真实 Verifier 对真实篡改/未知 key/吊销 key/回滚/过期/吊销版本产生的错误，经 Run.finish 得到退出码 5 与稳定错误码。
func TestVerifyFailuresProduceExitCode5(t *testing.T) {
	k := newCLIKeys(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	newV := func(seen int64) *manifest.Verifier {
		return &manifest.Verifier{Trust: k.trust, Seen: fixedSeq(seen), Now: func() time.Time { return now }}
	}
	good := cliManifest(10, "2027-02-01T00:00:00Z")
	tampered := append([]byte(nil), good...)
	tampered[20] ^= 1

	cases := []struct {
		name string
		run  func() error
		code string
	}{
		{"篡改清单", func() error {
			_, err := newV(0).Verify(tampered, cliSig("rel-cli-a", k.active, good), manifest.VerifyOptions{Channel: "stable"})
			return err
		}, "verify.signature_invalid"},
		{"未知 key_id", func() error {
			_, err := newV(0).Verify(good, cliSig("rel-cli-s", k.stranger, good), manifest.VerifyOptions{Channel: "stable"})
			return err
		}, "verify.unknown_key_id"},
		{"被吊销 key", func() error {
			_, err := newV(0).Verify(good, cliSig("rel-cli-r", k.revoked, good), manifest.VerifyOptions{Channel: "stable"})
			return err
		}, "verify.key_revoked"},
		{"sequence 回滚", func() error {
			_, err := newV(11).Verify(good, cliSig("rel-cli-a", k.active, good), manifest.VerifyOptions{Channel: "stable"})
			return err
		}, "verify.sequence_rollback"},
		{"清单过期", func() error {
			old := cliManifest(10, "2026-11-20T00:00:00Z")
			_, err := newV(0).Verify(old, cliSig("rel-cli-a", k.active, old), manifest.VerifyOptions{Channel: "stable"})
			return err
		}, "verify.manifest_expired"},
		{"吊销版本", func() error {
			res, err := newV(0).Verify(good, cliSig("rel-cli-a", k.active, good), manifest.VerifyOptions{Channel: "stable"})
			if err != nil {
				return err
			}
			return res.Manifest.CheckTarget("node", "0.0.9")
		}, "verify.version_revoked"},
		{"通道不符", func() error {
			_, err := newV(0).Verify(good, cliSig("rel-cli-a", k.active, good), manifest.VerifyOptions{Channel: "beta"})
			return err
		}, "verify.channel_mismatch"},
		{"被包装的错误也识别", func() error {
			_, err := newV(0).Verify(tampered, cliSig("rel-cli-a", k.active, good), manifest.VerifyOptions{Channel: "stable"})
			return &Error{Exit: ExitGeneral, Code: "x.outer", Message: "outer", Cause: err} // *Error 优先：保持调用者自己的退出码
		}, "x.outer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.run()
			if err == nil {
				t.Fatal("应失败")
			}
			code, env := finishWith(t, err)
			wantExit, wantName := 5, "VERIFY_FAILED"
			if c.code == "x.outer" {
				wantExit, wantName = 1, "GENERAL"
			}
			if code != wantExit || env["exit_code"].(float64) != float64(wantExit) || env["exit_name"] != wantName {
				t.Fatalf("退出码 = %d %v，期望 %d %s", code, env["exit_name"], wantExit, wantName)
			}
			if got := errCode(env); got != c.code {
				t.Fatalf("错误码 = %q，期望 %q", got, c.code)
			}
			if env["ok"] != false {
				t.Fatal("ok 应为 false")
			}
		})
	}
	// 成功路径不产生错误。
	if _, err := newV(0).Verify(good, cliSig("rel-cli-a", k.active, good), manifest.VerifyOptions{Channel: "stable"}); err != nil {
		t.Fatal(err)
	}
}

// 映射穷尽：manifest 的每个失败分类都有明确的退出码；信任库损坏=3、用法=2、其余验证失败=5。
func TestVerifyErrorMappingIsExhaustive(t *testing.T) {
	for _, kind := range manifest.Kinds() {
		e := VerifyError(&manifest.Error{Kind: kind, Msg: "m"})
		want := ExitVerifyFailed
		switch kind {
		case manifest.KindTrustStore:
			want = ExitPrecondition
		case manifest.KindOptions:
			want = ExitUsage
		}
		if e.Exit != want {
			t.Errorf("%s → 退出码 %d，期望 %d", kind, e.Exit, want)
		}
		if e.Code == "" || e.Code == "verify.unknown" || e.Hint == "" {
			t.Errorf("%s 缺少稳定错误码或提示: %+v", kind, e)
		}
		if e.SystemState != "unchanged" {
			t.Errorf("%s 的 system_state 应为 unchanged", kind)
		}
	}
	if e := VerifyError(&manifest.Error{Kind: "brand_new", Msg: "m"}); e.Exit != ExitGeneral {
		t.Errorf("未知分类应落到通用失败而不是假装成功/验证失败: %+v", e)
	}
}

// --allow-expired-manifest 只能与 --offline 同用（在线路径永不放行过期清单）。
func TestAllowExpiredManifestRequiresOffline(t *testing.T) {
	for _, args := range [][]string{
		{"install", "server", "--allow-expired-manifest"},
		{"update", "node", "--allow-expired-manifest"},
		{"update", "--all", "--allow-expired-manifest"},
	} {
		h := newHarness(t, asRoot())
		if code := h.run(append(args, "--json")...); code != 2 {
			t.Fatalf("%v：退出码 %d，期望 2", args, code)
		}
		if got := errCode(h.envelope()); got != "usage.flag_requires" {
			t.Fatalf("%v：错误码 %q", args, got)
		}
	}
	// 带 --offline 时通过用法校验，落到「尚未实现」（退出码 1，不是假成功）。
	for _, args := range [][]string{
		{"install", "server", "--offline", "/tmp/b.tar", "--allow-expired-manifest"},
		{"update", "node", "--offline", "/tmp/b.tar", "--allow-expired-manifest"},
	} {
		h := newHarness(t, asRoot())
		if code := h.run(append(args, "--json")...); code != 1 {
			t.Fatalf("%v：退出码 %d，期望 1（尚未实现）", args, code)
		}
	}
}

// 生产装配 Env.NewVerifier：内置公钥 + trust/keys.d + trust/state（真实文件）端到端；回滚由落盘的 sequence 触发，经 Run.finish 得到退出码 5。
func TestEnvNewVerifierEndToEnd(t *testing.T) {
	k := newCLIKeys(t)
	trustDir := t.TempDir() + "/trust"
	builtin, _ := json.Marshal(map[string]any{"schema_version": 1, "revoked_key_ids": []string{}, "keys": []any{
		map[string]any{"key_id": "rel-cli-a", "alg": "ed25519", "status": "active", "public_key": base64.StdEncoding.EncodeToString(k.active.Public().(ed25519.PublicKey))}}})
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	env := Env{TrustDir: trustDir, Now: func() time.Time { return now }, BuiltinKeys: func() ([]byte, error) { return builtin, nil }}

	v, err := env.NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	m10 := cliManifest(10, "2027-02-01T00:00:00Z")
	res, err := v.Verify(m10, cliSig("rel-cli-a", k.active, m10), manifest.VerifyOptions{Channel: "stable"})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Commit(res, env.KeysDir()); err != nil {
		t.Fatal(err)
	}
	// 新进程（新 Verifier）读取落盘的 sequence：sequence 9 被拒绝 → 退出码 5。
	v2, err := env.NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	m9 := cliManifest(9, "2027-02-01T00:00:00Z")
	_, err = v2.Verify(m9, cliSig("rel-cli-a", k.active, m9), manifest.VerifyOptions{Channel: "stable"})
	code, e := finishWith(t, err)
	if code != 5 || errCode(e) != "verify.sequence_rollback" {
		t.Fatalf("落盘后的回滚应得到退出码 5，实际 %d %v", code, e)
	}
	// 未注入内置公钥的构建：失败关闭，退出码 5（不是 panic、不是放行）。
	_, err = Env{TrustDir: trustDir, Now: env.Now}.NewVerifier()
	if _, ok := manifest.BuiltinKeys(); ok == nil {
		t.Skip("本地已注入内置公钥（发行构建环境）")
	}
	code, e = finishWith(t, err)
	if code != 5 || errCode(e) != "verify.no_builtin_keys" {
		t.Fatalf("无内置公钥应失败关闭，实际 %d %v", code, e)
	}
	// 信任库被破坏（state 损坏）：退出码 3，且不会把它当作「从未见过」。
	if err := os.WriteFile(trustDir+"/state", []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	v3, _ := env.NewVerifier()
	_, err = v3.Verify(m10, cliSig("rel-cli-a", k.active, m10), manifest.VerifyOptions{Channel: "stable"})
	code, e = finishWith(t, err)
	if code != 3 || errCode(e) != "trust.store_invalid" {
		t.Fatalf("损坏的信任状态应得到退出码 3，实际 %d %v", code, e)
	}
}

// cli 单测用到的测试公钥必须登记到 release-keys 门禁的指纹表（遗漏则生产构建可能内置它而不被发现）。
func TestCLITestKeysRegisteredWithReleaseKeysGate(t *testing.T) {
	b, err := os.ReadFile("../../../scripts/gates/data/test-release-pubkeys.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"active", "revoked", "stranger"} {
		pub := base64.StdEncoding.EncodeToString(cliTestKey(l).Public().(ed25519.PublicKey))
		if !bytes.Contains(b, []byte(pub)) {
			t.Errorf("cli 测试公钥 %q（%s）未登记到 scripts/gates/data/test-release-pubkeys.txt", l, pub)
		}
	}
}
