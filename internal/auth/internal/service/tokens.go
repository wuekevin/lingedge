package service

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// 会话令牌与 CSRF 令牌（ADR-018 §2/§3）。
//
//   - 会话令牌：32 字节 CSPRNG，Cookie 里是它的 base64url（无填充）文本；库里只存 SHA-256(32 字节原值) 的十六进制（sessions.token_hash）。
//   - CSRF 令牌：由会话令牌派生，csrf = base64url(HMAC-SHA256(key=会话令牌原值, msg="lingedge/csrf/v1"))；库里存 SHA-256(csrf 文本)（sessions.csrf_hash）。
//     这样 /auth/me、/auth/csrf 与登录响应对同一会话返回同一个令牌（无需另存明文），而服务端校验时按 csrf_hash 比对。
//     攻击者拿不到 HttpOnly 的会话令牌，就算不出 CSRF 令牌；拿到会话令牌本身已是会话劫持，CSRF 不再是额外边界。
const (
	tokenBytes = 32
	csrfMsg    = "lingedge/csrf/v1"
)

// ErrBadToken 表示 Cookie 里的令牌格式不对（不是 32 字节的 base64url）。
var ErrBadToken = errors.New("auth: 会话令牌格式无效")

// NewSessionToken 生成新的会话令牌，返回 Cookie 文本与库内哈希。
func NewSessionToken() (token, hash string, err error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("auth: 生成会话令牌: %w", err)
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(raw), hex.EncodeToString(sum[:]), nil
}

// HashSessionToken 校验令牌格式并返回库内哈希与原值（原值用于派生 CSRF）。
func HashSessionToken(token string) (hash string, raw []byte, err error) {
	if len(token) != base64.RawURLEncoding.EncodedLen(tokenBytes) {
		return "", nil, ErrBadToken
	}
	raw, err = base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != tokenBytes {
		return "", nil, ErrBadToken
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), raw, nil
}

// DeriveCSRF 由会话令牌原值派生 CSRF 令牌文本，并返回它的库内哈希。
func DeriveCSRF(rawToken []byte) (csrf, hash string) {
	m := hmac.New(sha256.New, rawToken)
	m.Write([]byte(csrfMsg))
	csrf = base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	return csrf, HashCSRF(csrf)
}

// HashCSRF 是 CSRF 令牌文本的库内哈希。
func HashCSRF(csrf string) string {
	sum := sha256.Sum256([]byte(csrf))
	return hex.EncodeToString(sum[:])
}

// CSRFMatches 以常量时间比较请求头里的 CSRF 令牌与库内哈希。
func CSRFMatches(header, storedHash string) bool {
	if header == "" || len(header) > 256 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(HashCSRF(header)), []byte(storedHash)) == 1
}
