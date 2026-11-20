package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func randomText(t testing.TB) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func newTestHasher(t testing.TB) *Hasher {
	t.Helper()
	h, err := NewHasher(Params{MemoryKiB: 19456, Iterations: 2, Parallelism: 1}, 2)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// AC1：argon2id，PHC 串格式；每次哈希盐不同；校验正确/错误；参数不同的旧哈希可校验并提示升级。
func TestHasherPHC(t *testing.T) {
	h, ctx := newTestHasher(t), context.Background()
	pw := randomText(t)
	a, err := h.Hash(ctx, pw)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := h.Hash(ctx, pw)
	if !strings.HasPrefix(a, "$argon2id$v=19$m=19456,t=2,p=1$") || strings.Count(a, "$") != 5 {
		t.Errorf("不是 argon2id PHC 串: %q", a)
	}
	if a == b {
		t.Error("同一口令两次哈希应因随机盐而不同")
	}
	if strings.Contains(a, pw) {
		t.Error("哈希串不得含明文")
	}
	ok, rehash, err := h.Verify(ctx, pw, a)
	if err != nil || !ok || rehash {
		t.Errorf("正确口令应通过且无需升级: ok=%v rehash=%v err=%v", ok, rehash, err)
	}
	if ok, _, err := h.Verify(ctx, pw+"x", a); err != nil || ok {
		t.Errorf("错误口令应不通过: ok=%v err=%v", ok, err)
	}
	// 旧参数的哈希：可校验，且提示升级；升级后的新哈希不再提示。
	old, _ := NewHasher(Params{MemoryKiB: 8192, Iterations: 1, Parallelism: 1}, 1)
	oh, _ := old.Hash(ctx, pw)
	if ok, rehash, err := h.Verify(ctx, pw, oh); err != nil || !ok || !rehash {
		t.Errorf("旧参数哈希应通过并提示升级: ok=%v rehash=%v err=%v", ok, rehash, err)
	}
	if ok, rehash, _ := h.Verify(ctx, pw+"x", oh); ok || rehash {
		t.Error("口令错误时不得提示升级")
	}
}

// 来自数据库的哈希串可能被篡改：格式错误、极端参数一律拒绝（ErrBadHash），不执行计算。
func TestVerifyRejectsMalformedHashes(t *testing.T) {
	h, ctx := newTestHasher(t), context.Background()
	for name, s := range map[string]string{
		"空":        "",
		"明文":       "hunter",
		"bcrypt":   "$2a$10$abcdefghijklmnopqrstuuABCDEFGHIJKLMNOPQRSTUVWXYZ012345",
		"argon2i":  "$argon2i$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"版本错误":     "$argon2id$v=16$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"内存过大":     "$argon2id$v=19$m=4194304,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"迭代过大":     "$argon2id$v=19$m=19456,t=1000000,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"并行过大":     "$argon2id$v=19$m=19456,t=2,p=200$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"盐太短":      "$argon2id$v=19$m=19456,t=2,p=1$c2E$aGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"缺字段":      "$argon2id$v=19$m=19456,t=2$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"非 base64": "$argon2id$v=19$m=19456,t=2,p=1$!!!!$????",
	} {
		start := time.Now()
		ok, _, err := h.Verify(ctx, "x", s)
		if ok || err != ErrBadHash {
			t.Errorf("%s: 应返回 ErrBadHash，实际 ok=%v err=%v", name, ok, err)
		}
		if time.Since(start) > 500*time.Millisecond {
			t.Errorf("%s: 拒绝应是即时的（不得执行 argon2 计算）", name)
		}
	}
}

// 并发哈希数受信号量限制：等待受 ctx 约束，取消后返回而不是无限阻塞。
func TestHasherConcurrencyLimitHonoursContext(t *testing.T) {
	h, _ := NewHasher(Params{MemoryKiB: 19456, Iterations: 2, Parallelism: 1}, 1)
	h.sem <- struct{}{} // 占满唯一的名额
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := h.Hash(ctx, "x"); err == nil {
		t.Fatal("名额被占满且 ctx 超时，应返回错误")
	}
	<-h.sem
	if _, err := h.Hash(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
}

func TestNewHasherRejectsBadParams(t *testing.T) {
	for _, p := range []Params{{}, {MemoryKiB: 1, Iterations: 1, Parallelism: 1}, {MemoryKiB: 19456, Iterations: 0, Parallelism: 1}, {MemoryKiB: 19456, Iterations: 1, Parallelism: 0}} {
		if _, err := NewHasher(p, 1); err == nil {
			t.Errorf("参数 %+v 应被拒绝", p)
		}
	}
}

// AC2：会话令牌 32 字节 CSPRNG；库内存 SHA-256；格式非法立即拒绝；CSRF 由令牌派生、稳定、可校验。
func TestSessionAndCSRFTokens(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		tok, hash, err := NewSessionToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[tok] {
			t.Fatal("令牌重复")
		}
		seen[tok] = true
		h2, raw, err := HashSessionToken(tok)
		if err != nil || h2 != hash || len(raw) != 32 || len(hash) != 64 {
			t.Fatalf("哈希不一致或长度不符: %v %q %q", err, h2, hash)
		}
	}
	for _, bad := range []string{"", "short", strings.Repeat("A", 43) + "=", strings.Repeat("!", 43), strings.Repeat("A", 44)} {
		if _, _, err := HashSessionToken(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	tok, _, _ := NewSessionToken()
	_, raw, _ := HashSessionToken(tok)
	c1, h1 := DeriveCSRF(raw)
	c2, h2 := DeriveCSRF(raw)
	if c1 != c2 || h1 != h2 || c1 == tok {
		t.Error("CSRF 应由令牌确定性派生，且不等于令牌本身")
	}
	if !CSRFMatches(c1, h1) || CSRFMatches("", h1) || CSRFMatches(c1+"x", h1) || CSRFMatches(strings.Repeat("a", 300), h1) {
		t.Error("CSRF 校验不符")
	}
	tok2, _, _ := NewSessionToken()
	_, raw2, _ := HashSessionToken(tok2)
	if other, _ := DeriveCSRF(raw2); CSRFMatches(other, h1) {
		t.Error("他人的 CSRF 令牌不得通过")
	}
}

func TestNormalizeEmailAndTrunc(t *testing.T) {
	if got := NormalizeEmail("  Foo@Example.COM "); got != "foo@example.com" {
		t.Errorf("NormalizeEmail = %q", got)
	}
	if s := trunc("你好世界", 4); s != "你" {
		t.Errorf("trunc 不得截断多字节字符: %q", s)
	}
}
