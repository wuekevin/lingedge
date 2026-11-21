package signing

import "time"

// CAExpiryWarnWindow 是私有 CA 剩余有效期告警阈值：剩余不足 1 年即告警（ADR-003 §3，tools-design 的 doctor 检查）。
const CAExpiryWarnWindow = 365 * 24 * time.Hour

// CAExpiryLevel 是 CA 有效期检查的结论。
type CAExpiryLevel string

// 检查结论。
const (
	CAExpiryOK      CAExpiryLevel = "ok"
	CAExpiryWarn    CAExpiryLevel = "warn"    // 剩余不足 1 年
	CAExpiryExpired CAExpiryLevel = "expired" // 已过期
)

// CAExpiryStatus 是 CA 证书有效期的检查结果。
type CAExpiryStatus struct {
	Level     CAExpiryLevel
	NotAfter  time.Time
	Remaining time.Duration // 已过期时为负
}

// CheckCAExpiry 报告私有 CA 证书的剩余有效期：剩余 < 1 年为 warn，已过期为 expired。now 由调用方给出（本包不读取时钟）。
// 这是供 Tools `doctor`（Story 1.42）与 Server 复用的纯函数：doctor 读取 CA 证书的 NotAfter 后调用它。
func CheckCAExpiry(notAfter, now time.Time) CAExpiryStatus {
	rem := notAfter.Sub(now)
	st := CAExpiryStatus{Level: CAExpiryOK, NotAfter: notAfter, Remaining: rem}
	switch {
	case rem <= 0:
		st.Level = CAExpiryExpired
	case rem < CAExpiryWarnWindow:
		st.Level = CAExpiryWarn
	}
	return st
}
