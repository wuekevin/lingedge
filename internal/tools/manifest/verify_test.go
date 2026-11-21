package manifest

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"
)

func (e *env) verify(raw, sig []byte, opt VerifyOptions) (*Result, error) {
	if opt.Channel == "" {
		opt.Channel = "stable"
	}
	return e.v.Verify(raw, sig, opt)
}

func TestVerifyHappyPath(t *testing.T) {
	e := newEnv(t)
	raw := mf(t, nil)
	res, err := e.verify(raw, signManifest(e.active, raw), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.KeyID != "rel-test-a" || res.Manifest.Sequence != 10 || len(res.Warnings) != 0 {
		t.Fatalf("结果异常: %+v", res)
	}
	if _, ok := res.Manifest.Components["server"]; !ok {
		t.Fatal("缺少 server 组件")
	}
	// 退役公钥在重叠期内仍可验证（§9.1）。
	old := deriveKey("rel-test-old", "old")
	if _, err := e.verify(raw, signManifest(old, raw), VerifyOptions{}); err != nil {
		t.Fatalf("retired key 应仍可验证: %v", err)
	}
}

func TestVerifyTamperedManifest(t *testing.T) {
	e := newEnv(t)
	raw := mf(t, nil)
	sig := signManifest(e.active, raw)
	// 逐字节翻转：每个位置翻 1 位都必须失败（任一输入字节翻转 → 失败，§9.6）。
	for i := 0; i < len(raw); i++ {
		bad := append([]byte(nil), raw...)
		bad[i] ^= 0x01
		_, err := e.verify(bad, sig, VerifyOptions{})
		if err == nil {
			t.Fatalf("翻转第 %d 字节后验签仍通过", i)
		}
		if k := err.(*Error).Kind; k != KindSignature && k != KindSchema {
			t.Fatalf("翻转第 %d 字节：分类 %s", i, k)
		}
	}
	// 追加一个字节（含空白）同样失败：签名覆盖原始字节而非规范化后的 JSON。
	_, err := e.verify(append(append([]byte(nil), raw...), ' '), sig, VerifyOptions{})
	wantKind(t, err, KindSignature)
}

func TestVerifyTamperedSignature(t *testing.T) {
	e := newEnv(t)
	raw := mf(t, nil)
	sig := signManifest(e.active, raw)
	// 签名翻位。
	sf, _ := ParseSig(sig)
	bad := append([]byte(nil), sf.sig...)
	bad[0] ^= 1
	_, err := e.verify(raw, sigDoc("rel-test-a", bad), VerifyOptions{})
	wantKind(t, err, KindSignature)
	// 签名长度不对、非 base64、算法不对、多余字段、空文件。
	for name, s := range map[string]string{
		"short":    `{"key_id":"rel-test-a","alg":"ed25519","sig":"AAAA"}`,
		"notb64":   `{"key_id":"rel-test-a","alg":"ed25519","sig":"!!!"}`,
		"alg":      `{"key_id":"rel-test-a","alg":"rsa","sig":"` + strings.Repeat("A", 86) + `=="}`,
		"extra":    `{"key_id":"rel-test-a","alg":"ed25519","sig":"` + strings.Repeat("A", 86) + `==","x":1}`,
		"empty":    ``,
		"notjson":  `rel-test-a`,
		"trailing": string(sig) + `{}`,
	} {
		_, err := e.verify(raw, []byte(s), VerifyOptions{})
		if err == nil {
			t.Fatalf("%s：应失败", name)
		}
		wantKind(t, err, KindSignature)
	}
	// 用另一把可信公钥的签名冒充 key_id。
	old := deriveKey("rel-test-a", "old") // id 相同、私钥不同
	_, err = e.verify(raw, signManifest(old, raw), VerifyOptions{})
	wantKind(t, err, KindSignature)
	// 签名对象缺少域前缀（裸签名）必须失败：域分隔。
	naked := sigDoc("rel-test-a", ed25519.Sign(e.active.priv, raw))
	_, err = e.verify(raw, naked, VerifyOptions{})
	wantKind(t, err, KindSignature)
}

func TestVerifyUnknownAndRevokedKey(t *testing.T) {
	e := newEnv(t)
	raw := mf(t, nil)
	stranger := deriveKey("rel-test-unknown", "stranger")
	_, err := e.verify(raw, signManifest(stranger, raw), VerifyOptions{})
	wantKind(t, err, KindUnknownKey)

	// 被 revoked_key_ids 吊销的 key：即使签名数学上正确也拒绝。
	rev := deriveKey("rel-test-x", "x")
	_, err = e.verify(raw, signManifest(rev, raw), VerifyOptions{})
	wantKind(t, err, KindKeyRevoked)

	// 同一把 key 同时出现在 keys 与 revoked_key_ids：吊销优先（status 写 active 也一样）。
	bf, _ := ParseKeysFile(keysDoc(t, []string{"rel-test-a"}, e.active.entry(StatusActive, "", ""), deriveKey("rel-test-b", "b").entry(StatusActive, "", "")))
	ts, err := NewTrustSet(bf)
	if err != nil {
		t.Fatal(err)
	}
	v2 := &Verifier{Trust: ts, Seen: memSeq{}, Now: e.v.Now}
	_, err = v2.Verify(raw, signManifest(e.active, raw), VerifyOptions{Channel: "stable"})
	wantKind(t, err, KindKeyRevoked)

	// status=revoked 的条目等同吊销。
	bf2, _ := ParseKeysFile(keysDoc(t, nil, e.active.entry(StatusRevoked, "", ""), deriveKey("rel-test-b", "b").entry(StatusActive, "", "")))
	ts2, err := NewTrustSet(bf2)
	if err != nil {
		t.Fatal(err)
	}
	v3 := &Verifier{Trust: ts2, Seen: memSeq{}, Now: e.v.Now}
	_, err = v3.Verify(raw, signManifest(e.active, raw), VerifyOptions{Channel: "stable"})
	wantKind(t, err, KindKeyRevoked)
}

func TestVerifyKeyWindowUsesGeneratedAt(t *testing.T) {
	e := newEnv(t)
	// 公钥窗口 [2026-01-01, 2028-01-01)：generated_at 在窗口外即拒绝，与「现在」无关。
	early := mf(t, func(m map[string]any) {
		m["generated_at"] = "2025-12-31T23:59:59Z"
		m["valid_until"] = "2027-02-01T00:00:00Z"
	})
	_, err := e.verify(early, signManifest(e.active, early), VerifyOptions{})
	wantKind(t, err, KindKeyInvalid)
	late := mf(t, func(m map[string]any) {
		m["generated_at"] = "2028-01-01T00:00:00Z"
		m["valid_until"] = "2029-01-01T00:00:00Z"
	})
	_, err = e.verify(late, signManifest(e.active, late), VerifyOptions{})
	wantKind(t, err, KindKeyInvalid)
	// 边界：恰好 not_before 通过。
	edge := mf(t, func(m map[string]any) {
		m["generated_at"] = "2026-01-01T00:00:00Z"
		m["valid_until"] = "2027-02-01T00:00:00Z"
	})
	if _, err := e.verify(edge, signManifest(e.active, edge), VerifyOptions{}); err != nil {
		t.Fatalf("generated_at == not_before 应通过: %v", err)
	}
}

func TestVerifyRollback(t *testing.T) {
	e := newEnv(t)
	e.seen["stable"] = 11
	raw := mf(t, nil) // sequence 10 < 11
	_, err := e.verify(raw, signManifest(e.active, raw), VerifyOptions{})
	wantKind(t, err, KindRollback)

	// 相等允许（幂等重取），大于允许。
	e.seen["stable"] = 10
	if _, err := e.verify(raw, signManifest(e.active, raw), VerifyOptions{}); err != nil {
		t.Fatalf("sequence 相等应允许: %v", err)
	}
	e.seen["stable"] = 9
	if _, err := e.verify(raw, signManifest(e.active, raw), VerifyOptions{}); err != nil {
		t.Fatalf("sequence 更大应允许: %v", err)
	}
	// 通道独立：beta 的 sequence 不影响 stable。
	e.seen["stable"], e.seen["beta"] = 0, 999
	delete(e.seen, "stable")
	if _, err := e.verify(raw, signManifest(e.active, raw), VerifyOptions{}); err != nil {
		t.Fatalf("beta 的已见值不应阻断 stable: %v", err)
	}
	// 通道不符。
	_, err = e.verify(raw, signManifest(e.active, raw), VerifyOptions{Channel: "beta"})
	wantKind(t, err, KindChannel)
	// 回滚拒绝时不得写入任何状态；成功校验本身也不写（只有 Commit 才写）。
	if _, ok := e.seen["stable"]; ok {
		t.Fatal("Verify 不得修改 sequence 状态")
	}
}

func TestVerifyExpiry(t *testing.T) {
	e := newEnv(t)
	raw := mf(t, nil) // valid_until 2027-02-01
	sig := signManifest(e.active, raw)
	vu := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)

	e.now = vu.Add(-time.Nanosecond)
	if _, err := e.verify(raw, sig, VerifyOptions{}); err != nil {
		t.Fatalf("valid_until 前一刻应通过: %v", err)
	}
	e.now = vu // 恰等于：已过期
	_, err := e.verify(raw, sig, VerifyOptions{})
	wantKind(t, err, KindExpired)
	e.now = vu.Add(24 * time.Hour)
	_, err = e.verify(raw, sig, VerifyOptions{})
	wantKind(t, err, KindExpired)

	// --offline + --allow-expired-manifest：放行并告警。
	res, err := e.verify(raw, sig, VerifyOptions{Offline: true, AllowExpired: true})
	if err != nil {
		t.Fatalf("显式放行应通过: %v", err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "过期") {
		t.Fatalf("应有过期告警: %v", res.Warnings)
	}
	// 仅 --offline 不放行；--allow-expired-manifest 不带 --offline 是用法错误（不是静默放行）。
	_, err = e.verify(raw, sig, VerifyOptions{Offline: true})
	wantKind(t, err, KindExpired)
	_, err = e.verify(raw, sig, VerifyOptions{AllowExpired: true})
	wantKind(t, err, KindOptions)
	// 放行过期不会放行回滚：sequence 与签名检查仍生效。
	e.seen["stable"] = 11
	_, err = e.verify(raw, sig, VerifyOptions{Offline: true, AllowExpired: true})
	wantKind(t, err, KindRollback)
	e.seen["stable"] = 0
	delete(e.seen, "stable")
	bad := append([]byte(nil), raw...)
	bad[10] ^= 1
	_, err = e.verify(bad, sig, VerifyOptions{Offline: true, AllowExpired: true})
	wantKind(t, err, KindSignature)
	// 未过期时 AllowExpired 不产生告警。
	e.now = t0
	res, err = e.verify(raw, sig, VerifyOptions{Offline: true, AllowExpired: true})
	if err != nil || len(res.Warnings) != 0 {
		t.Fatalf("未过期不应告警: %v %v", err, res)
	}
}

func TestVerifyRevokedVersions(t *testing.T) {
	e := newEnv(t)
	raw := mf(t, nil)
	res, err := e.verify(raw, signManifest(e.active, raw), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// 命中（含构建元数据变体）→ 不可作目标。
	for _, v := range []string{"0.0.9", "0.0.9+build.5"} {
		wantKind(t, res.Manifest.CheckTarget("node", v), KindVersionRevoked)
	}
	// 未命中：不同版本、同版本不同组件、预发布。
	for _, c := range [][2]string{{"node", "0.1.0"}, {"server", "0.0.9"}, {"node", "0.0.9-rc.1"}, {"node", "0.0.10"}} {
		if err := res.Manifest.CheckTarget(c[0], c[1]); err != nil {
			t.Fatalf("%v 不应被吊销: %v", c, err)
		}
	}
	// 版本号非法 → 拒绝。
	wantKind(t, res.Manifest.CheckTarget("node", "v0.0.9"), KindSchema)
	if reason, rev := res.Manifest.RevokedVersion("node", mustVer(t, "0.0.9")); !rev || reason == "" {
		t.Fatalf("应返回吊销原因: %q %v", reason, rev)
	}
}

func mustVer(t *testing.T, s string) Version {
	t.Helper()
	v, err := ParseVersion(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerifyOptionsValidation(t *testing.T) {
	e := newEnv(t)
	raw := mf(t, nil)
	sig := signManifest(e.active, raw)
	_, err := e.v.Verify(raw, sig, VerifyOptions{})
	wantKind(t, err, KindOptions)
	_, err = (&Verifier{}).Verify(raw, sig, VerifyOptions{Channel: "stable"})
	wantKind(t, err, KindOptions)
	_, err = e.verify(nil, sig, VerifyOptions{})
	wantKind(t, err, KindSchema)
}

func TestCommitAdvancesSequenceOnlyUpward(t *testing.T) {
	e := newEnv(t)
	raw := mf(t, nil)
	res, err := e.verify(raw, signManifest(e.active, raw), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := e.v.Commit(res, dir+"/keys.d"); err != nil {
		t.Fatal(err)
	}
	if e.seen["stable"] != 10 {
		t.Fatalf("Commit 应推进 sequence: %v", e.seen)
	}
	e.seen["stable"] = 20
	_ = e.v.Commit(res, dir+"/keys.d")
	if e.seen["stable"] != 20 {
		t.Fatal("sequence 不得回退")
	}
}

// 防御纵深：即便信任集里混入了「status=revoked 但不在吊销集」的条目（NewTrustSet 正常不会产生），Verify 仍拒绝使用它。
func TestVerifyRejectsKeyWithNonVerifiableStatus(t *testing.T) {
	e := newEnv(t)
	k := e.trust.keys[e.active.id]
	k.Status = StatusRevoked
	e.trust.keys[e.active.id] = k
	raw := mf(t, nil)
	_, err := e.verify(raw, signManifest(e.active, raw), VerifyOptions{})
	wantKind(t, err, KindKeyInvalid)
	_, err = e.v.VerifyPackageSums(signManifest(e.active, raw), []byte("x"), "node", "1.0.0", "any", t0)
	wantKind(t, err, KindKeyInvalid)
}
