package main

import (
	"bytes"
	"encoding/base64"
	"math"
	"path"
	"regexp"
	"strings"
)

// 仓库密钥扫描（G-7 的「密钥扫描」部分，Story 1.34 AC2、ADR-012 §1.5/验证）：**仓库与 CI 中不存在任何发行私钥**。
// 威胁模型 G-7 的其余部分（依赖漏洞 `make vuln`、SBOM 随发行流水线产出）见 Makefile 与 docs/operations/release.md。
//
// 与 G-6（固定口令）并列且**不设任何豁免**：测试用的发行私钥一律在测试运行时随机生成（内存或临时目录），不得以任何形态入库。
//
//	secret-pem-private        PEM 私钥块（RSA/EC/OpenSSH/PKCS#8/加密私钥/PGP），**且正文是真实密钥结构**（见 pemBodyIsRealKey：DER 外层长度自洽、OpenSSH 魔数、PGP 足够长）——
//	                          仓库里为脱敏/解析测试写的「PEM 头 + 占位正文」（如 MIIB、AAAA、MIIFAKEFAKE…）不是私钥，不触发；真实私钥一律命中；
//	secret-release-key-file   离线签名工具（scripts/release-sign）的私钥文件格式（format 标识 + seed 字段）；
//	secret-pkcs8-ed25519      Ed25519 的 PKCS#8 私钥 DER 的 base64 形态（无 PEM 头也能识别）；
//	secret-private-key-field  名为 private_key/secret_key/signing_key/release_key 的键被赋予「种子/私钥长度」的高熵字面量（44 字符 base64=32 字节、88 字符=64 字节、64/128 位十六进制、≥160 字符 base64）；
//	secret-key-filename       私钥类文件名（*.key *.p12 *.pfx *.jks *.keystore、id_rsa/id_ed25519/id_ecdsa、release-key*.json）；
//	secret-ci-release-key     CI 配置里引用发行私钥/离线签名（发行私钥不得在 CI 中可达，ADR-012 §1.5）。
//
// 已知边界（如实记录，不是豁免）：本门禁识别「真实私钥形态」，不试图把任意 32 字节十六进制都当私钥——§9.6 公开示例向量
// （internal/tools/manifest/testdata/vectors.json 的 seed_hex 与 tools-design §9.6）是文档化的**公开测试种子**，其公钥已登记在
// data/test-release-pubkeys.txt，G-11 与 release-keys 门禁保证它永不进入生产构建。
// 门禁自身的规则文本位于 scripts/gates（被扫描器跳过），负向样本在 secrets_test.go 里于运行时拼接生成。

var (
	reSecretPEM     = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
	reSecretKeyFile = regexp.MustCompile(`(?s)lingedge-release-key-v1.{0,300}?"seed"\s*:\s*"[A-Za-z0-9+/]{43}=?"`)
	reSecretPKCS8   = regexp.MustCompile(`MC4CAQAwBQYDK2VwBCIEI[A-Za-z0-9+/]{43}`)
	reSecretField   = regexp.MustCompile(`(?i)["']?\b(?:private[_-]?key|secret[_-]?key|signing[_-]?key|release[_-]?key)["']?\s*(?::=|[:=])\s*["']?([A-Za-z0-9+/_=-]{43,})["']?`)
	reSecretCI      = regexp.MustCompile(`(?i)release[_-]?(?:signing[_-]?)?(?:private[_-]?)?key|RELEASE_SIGN|\brelease-sign\b`)

	secretNameRes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\.(key|p12|pfx|jks|keystore)$`),
		regexp.MustCompile(`(?i)^id_(rsa|ed25519|ecdsa|dsa)$`),
		regexp.MustCompile(`(?i)^release[-_.]?(signing[-_.]?)?key.*\.json$`),
	}
)

// entropy 返回字符串的香农熵（比特/字符）。
func entropy(s string) float64 {
	if s == "" {
		return 0
	}
	cnt := map[rune]float64{}
	for _, r := range s {
		cnt[r]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range cnt {
		p := c / n
		h -= p * math.Log2(p)
	}
	return h
}

func isCIConfig(rel string) bool {
	return strings.HasPrefix(rel, ".github/") || strings.HasPrefix(rel, ".gitlab-ci") || strings.HasPrefix(rel, "Jenkinsfile") || strings.HasPrefix(rel, ".circleci/")
}

// secretFileNameFinding 报告私钥类文件名。
func secretFileNameFinding(rel string) *Finding {
	base := path.Base(rel)
	for _, re := range secretNameRes {
		if re.MatchString(base) {
			return &Finding{File: rel, Rule: "secret-key-filename", Msg: "私钥类文件名：仓库与 CI 中不得存在发行/签名私钥（测试密钥须运行时生成，不得入库；G-7、ADR-012 §1.5）"}
		}
	}
	return nil
}

// scanSecretText 扫描一个文本文件的内容。
func scanSecretText(rel string, content []byte) []Finding {
	var out []Finding
	add := func(off int, rule, msg string) {
		out = append(out, Finding{File: rel, Line: lineOf(content, off), Rule: rule, Msg: msg})
	}
	for _, loc := range reSecretPEM.FindAllIndex(content, -1) {
		tail := content[loc[1]:]
		if len(tail) > 16<<10 {
			tail = tail[:16<<10]
		}
		if pemBodyIsRealKey(string(content[loc[0]:loc[1]]), tail) {
			add(loc[0], "secret-pem-private", "PEM 私钥块（正文是真实密钥结构）：私钥不得出现在仓库里（测试密钥运行时生成；G-7）")
		}
	}
	for _, loc := range reSecretKeyFile.FindAllIndex(content, -1) {
		add(loc[0], "secret-release-key-file", "离线签名工具私钥文件内容：发行私钥只存在于离线机（ADR-012 §1.5）")
	}
	for _, loc := range reSecretPKCS8.FindAllIndex(content, -1) {
		add(loc[0], "secret-pkcs8-ed25519", "Ed25519 PKCS#8 私钥 DER（base64）：私钥不得出现在仓库里")
	}
	for _, m := range reSecretField.FindAllSubmatchIndex(content, -1) {
		if looksLikeKeyMaterial(string(content[m[2]:m[3]])) {
			add(m[0], "secret-private-key-field", "疑似私钥/签名密钥的高熵字面量被赋给 private_key/secret_key/signing_key/release_key（G-7）")
		}
	}
	if isCIConfig(rel) {
		for _, loc := range reSecretCI.FindAllIndex(content, -1) {
			add(loc[0], "secret-ci-release-key", "CI 配置引用发行私钥/离线签名：发行私钥不得在 CI 中可达（签名只在离线机，ADR-012 §1.5）")
		}
	}
	return out
}

func runSecrets(root string, _ options) ([]Finding, error) {
	var out []Finding
	err := walkText(root, func(rel string, isDir bool) bool {
		if !isDir {
			if f := secretFileNameFinding(rel); f != nil {
				out = append(out, *f)
			}
		}
		return false
	}, func(f textFile) {
		out = append(out, scanSecretText(f.Rel, f.Content)...)
	})
	return out, err
}

var (
	reHexOnly = regexp.MustCompile(`^[0-9a-fA-F]+$`)
	reB64Only = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)
)

// looksLikeKeyMaterial 判断字面量是否是「种子/私钥长度」的高熵值（排除路径、占位符、短值与只有 DER 头的固定前缀）。
func looksLikeKeyMaterial(v string) bool {
	switch {
	case reHexOnly.MatchString(v) && (len(v) == 64 || len(v) == 128):
		return entropy(v) >= 3.2
	case reB64Only.MatchString(v) && ((len(v) == 44 && strings.HasSuffix(v, "=")) || (len(v) == 88 && strings.HasSuffix(v, "==")) || len(v) >= 160):
		return entropy(v) >= 4.0 && strings.ContainsAny(v, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") && strings.ContainsAny(v, "abcdefghijklmnopqrstuvwxyz")
	}
	return false
}

// pemBodyIsRealKey 判断 PEM 头后的正文是否是真实密钥结构：
//   - OPENSSH：正文解码后以 "openssh-key-v1\0" 开头；
//   - PGP：base64 正文 ≥ 200 字符；
//   - 其他（RSA/EC/PKCS#8/ENCRYPTED）：解码得到 DER，且 DER 外层 SEQUENCE 的长度域与实际字节数自洽。
//
// 占位正文（过短、被截断、重复字样）不满足这些结构约束。
func pemBodyIsRealKey(header string, tail []byte) bool {
	body := tail
	if i := bytes.Index(body, []byte("-----END")); i >= 0 {
		body = body[:i]
	}
	txt := strings.ReplaceAll(string(body), `\n`, "\n")
	var b64 strings.Builder
	for _, line := range strings.Split(txt, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, ":") { // 头部字段（Proc-Type/DEK-Info/Version）
			continue
		}
		b64.WriteString(line)
	}
	raw := strings.TrimRight(b64.String(), "=")
	if len(raw) < 16 {
		return false
	}
	der, err := base64.RawStdEncoding.DecodeString(raw)
	if err != nil {
		return false
	}
	switch {
	case strings.Contains(header, "OPENSSH"):
		return bytes.HasPrefix(der, []byte("openssh-key-v1\x00"))
	case strings.Contains(header, "PGP"):
		return len(raw) >= 200
	}
	if len(der) < 16 || der[0] != 0x30 {
		return false
	}
	total := 0
	switch {
	case der[1] < 0x80:
		total = 2 + int(der[1])
	default:
		n := int(der[1] & 0x7f)
		if n < 1 || n > 4 || len(der) < 2+n {
			return false
		}
		l := 0
		for _, b := range der[2 : 2+n] {
			l = l<<8 | int(b)
		}
		total = 2 + n + l
	}
	return total == len(der)
}
