package redact

import "math"

// 裸 base64 密钥识别（Story 1.11 遗留项）：自由文本里「不带键名」的 32 字节随机密钥（主密钥/KEK，LingEdge 主密钥即 32B）的 base64 文本形态。
//
// 为把误伤压到最低，规则刻意收窄，四项条件须同时满足：
//  1. 是一段「最大连续 base64 字符串」（字母数字与 _ - + /），其前后都不是该字符集的字符（因此长路径、URL 片段、点分主机名、
//     JWT 的各段都不会因拼接而变长；以 "k1:" 之类的分隔符开头的密钥行可被识别）；
//  2. 长度恰为 32 字节密钥的 43（std 变体可带 1 个 '=' 填充）——ULID（26）、UUID（36，含连字符）、
//     十六进制 SHA-1/SHA-256（40/64，且为单一字母大小写）、证书 PEM 行（64）、普通单词与路径段均不在此长度；
//  3. 同时含大写字母、小写字母与数字（单一大小写的十六进制散列、纯字母的长标识因此不会命中）；
//  4. 香农熵 ≥ 4.5 bit/字符（随机 base64 约 5.0–5.3；自然语言/驼峰标识约 3.5–4.2）。
//
// 已知并接受的取舍（见 docs/operations/secrets.md §7）：
//   - 十六进制主密钥（64 个十六进制字符）与证书/文件指纹的十六进制 SHA-256 无法区分，因此不做识别（单一大小写，也不满足条件 3）；
//   - 32 字节散列的 base64 形态（如 SHA-256 的 base64、HS256 的 JWT 签名段）与密钥无法区分，会被一并遮蔽（宁多遮不漏；当前代码库没有日志输出此类值）；
//   - 其他长度（16/24/64 字节密钥；64 字节的 86 字符形态与 RSA 公钥行等良性内容过于接近，不做）与被换行折断的 base64 不识别。
const (
	bareLenKey32   = 43
	bareLenKey64   = 86
	bareMinEntropy = 4.5
)

func isB64Byte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '+' || c == '/'
}

// maskBareKeys 把 s 中满足上述条件的裸 base64 密钥替换为 Mask；无命中时原样返回同一字符串（无分配）。
func maskBareKeys(s string) string {
	var out []byte
	last := 0
	for i := 0; i < len(s); {
		if !isB64Byte(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isB64Byte(s[j]) {
			j++
		}
		n := j - i
		pad := 0
		for pad < 2 && j+pad < len(s) && s[j+pad] == '=' {
			pad++
		}
		if (n == bareLenKey32 && pad <= 1) && looksRandom(s[i:j]) {
			if out == nil {
				out = make([]byte, 0, len(s))
			}
			out = append(out, s[last:i]...)
			out = append(out, Mask...)
			last = j + pad
			j += pad
		}
		i = j
	}
	if out == nil {
		return s
	}
	return string(append(out, s[last:]...))
}

func looksRandom(run string) bool {
	var upper, lower, digit bool
	var freq [256]int
	for i := 0; i < len(run); i++ {
		c := run[i]
		freq[c]++
		switch {
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= '0' && c <= '9':
			digit = true
		}
	}
	if !upper || !lower || !digit {
		return false
	}
	var h float64
	n := float64(len(run))
	for _, f := range freq {
		if f > 0 {
			p := float64(f) / n
			h -= p * math.Log2(p)
		}
	}
	return h >= bareMinEntropy
}
