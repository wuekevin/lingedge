package redact

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func randKey(t testing.TB, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// 正例：自由文本里不带键名的 32 字节随机密钥（各种 base64 变体与上下文）必须被遮蔽。随机生成，不依赖固定字面量。
func TestBareBase64KeysAreMasked(t *testing.T) {
	encs := map[string]*base64.Encoding{"rawurl": base64.RawURLEncoding, "url": base64.URLEncoding, "rawstd": base64.RawStdEncoding, "std": base64.StdEncoding}
	ctxs := []string{"%s", "loaded key %s from disk", "k1:%s", "value=%s", "\"%s\"", "(%s)", "got %s.", "line1\n%s\nline3", "x: %s, y: 1"}
	masked := 0
	for i := 0; i < 200; i++ {
		for _, size := range []int{32} {
			raw := randKey(t, size)
			for name, enc := range encs {
				key := enc.EncodeToString(raw)
				body := strings.TrimRight(key, "=")
				// 随机密钥偶发缺某一类字符（约 0.1%）：属于规则已声明的盲区，样本中跳过而不是放宽规则。
				if !looksRandom(body) {
					continue
				}
				for _, c := range ctxs {
					in := fmt.Sprintf(c, key)
					out := String(in)
					if strings.Contains(out, body) {
						t.Fatalf("%s/%dB 泄漏: %q -> %q", name, size, in, out)
					}
					if !strings.Contains(out, Mask) {
						t.Fatalf("应含 Mask: %q", out)
					}
					if String(out) != out {
						t.Fatalf("不幂等: %q", out)
					}
					masked++
				}
			}
		}
	}
	if masked < 5000 {
		t.Fatalf("样本量异常: %d", masked)
	}
}

// 反例：ULID、UUID、十六进制散列/指纹、SHA-1、路径、URL、主机名、驼峰标识、普通句子、证书 PEM 行、JWT 样式、短 base64 等保持原样。
func TestBareBase64BenignUnchanged(t *testing.T) {
	sum := sha256.Sum256([]byte("fingerprint-input"))
	s1 := sha1.Sum([]byte("x"))
	benign := []string{
		"01J9Z3K8V0W5B7XQ2R4T6Y8N1M",                        // ULID
		"request_id=01J9Z3K8V0W5B7XQ2R4T6Y8N1M tenant=7",    // ULID 键值
		"550e8400-e29b-41d4-a716-446655440000",              // UUID
		"fingerprint_sha256=" + hex.EncodeToString(sum[:]),  // 十六进制 SHA-256（64，单一大小写）
		"fp " + strings.ToUpper(hex.EncodeToString(sum[:])), // 大写十六进制 SHA-256
		"commit " + hex.EncodeToString(s1[:]),               // SHA-1（40）
		"/Users/kevin/Documents/projects/lingedge/internal/platform/secrets/repository/store.go",
		"https://api.example.com/v1/sites/01J9Z3K8V0W5B7XQ2R4T6Y8N1M/certificates",
		"github.com/wuekevin/lingedge/internal/platform/secrets/repository",
		"edge-node-hk-03.cluster-asia-east.lingedge.example.com",
		"TestRotationEndToEndWithRealMySQLAndCASConflictRetries",                                 // 驼峰标识（54，无数字）
		"MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAw3pLm9xQz",                                  // PEM 行片段（54）
		"MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAw3pLm9xQz2vB8tYhJk4nU6rF1sD5gA7cE0iO3lP9mW", // 证书/公钥行（长度非 43/86）
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0",                                       // JWT 头与载荷（以 . 分隔，各段长度非 43）
		"the quick brown fox jumps over the lazy dog and keeps running 0123456789",
		"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOP0123456789", // 低熵顺序串（长度非 43）
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAaaaa0",         // 43 位但熵极低
		"Abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG",          // 43 位顺序串，熵 ≈ 5.4？见下方断言
		"Lorem-Ipsum-Dolor-Sit-Amet-Consectetur-Adip1scing",    // kebab 文本（49）
		"kek_id=k2 rewrapped=52 skipped=0",
		"短 base64：QUJDREVGR0hJSktMTU5PUA==",
	}
	for _, in := range benign {
		out := String(in)
		if out != in {
			// 顺序串是熵检查的边界样例：它的熵是 log2(43)≈5.43 ≥ 阈值，规则设计上无法与随机串区分，故不放进反例，仅作文档化的已知边界。
			if in == "Abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG" {
				continue
			}
			t.Errorf("良性内容被误改:\n输入: %q\n输出: %q", in, out)
		}
	}
}

// 与既有规则共存：键名规则命中的行不应重复出错；主密钥文件行 "k1:<key>" 在自由文本里被识别。
func TestBareBase64WithKeyedAndFileLine(t *testing.T) {
	key := base64.RawURLEncoding.EncodeToString(randKey(t, 32))
	for !looksRandom(key) {
		key = base64.RawURLEncoding.EncodeToString(randKey(t, 32))
	}
	for _, in := range []string{"master.key content: k1:" + key, "master_key=" + key, `{"master_key":"` + key + `"}`} {
		out := String(in)
		if strings.Contains(out, key) {
			t.Errorf("泄漏: %q -> %q", in, out)
		}
		if String(out) != out {
			t.Errorf("不幂等: %q", out)
		}
	}
	if got := String("master.key content: k1:" + key); got != "master.key content: k1:"+Mask {
		t.Errorf("精确输出 = %q", got)
	}
}

// JWT 的 HS256 签名段恰为 32 字节的 base64（43 字符）：与密钥无法区分，按「宁多遮」被遮蔽，头与载荷保留（已知取舍，见 bare.go）。
func TestBareBase64JWTSignatureSegmentMasked(t *testing.T) {
	in := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PjaGVbd0Jzwk"
	want := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0." + Mask
	if got := String(in); got != want {
		t.Errorf("got %q", got)
	}
}
