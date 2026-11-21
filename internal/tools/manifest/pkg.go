package manifest

import (
	"crypto/ed25519"
	"encoding/json"
	"time"
)

// 包格式（tools-design §9.3，CF-6）：`.tar.gz`，内含 META/package.json、META/SHA256SUMS、META/SHA256SUMS.sig、payload/…。
// 清单签名已覆盖各包的 sha256；包内 SHA256SUMS 的签名对象把 component/version/platform 绑定进去，
// 防止把 A 组件/版本/平台的合法包体冒充成 B（SHA256SUMS 字节相同也不行）。

// PackageSigInput 返回包内签名输入："LINGEDGE-PKG-V1\n<component>\n<version>\n<platform>\n" ‖ SHA256SUMS 字节。
// 三个字段先经格式校验（不得含换行，否则签名输入有歧义）。
func PackageSigInput(component, version, platform string, sums []byte) ([]byte, error) {
	if !reComponent.MatchString(component) {
		return nil, newErr(KindPackage, "component %q 非法", trimTo(component, 32))
	}
	pv, err := ParseVersion(version)
	if err != nil {
		return nil, err
	}
	if !rePlatform.MatchString(platform) {
		return nil, newErr(KindPackage, "platform %q 非法", trimTo(platform, 32))
	}
	in := PackageDomain + component + "\n" + pv.String() + "\n" + platform + "\n"
	return append([]byte(in), sums...), nil
}

// VerifyPackageSums 验证 META/SHA256SUMS.sig（§9.5 第 6 步）：key_id 须在可信集（未吊销、active|retired），
// 且 `at`（调用方传入验证通过的清单 generated_at）在公钥有效期窗口内。返回签名所用 key_id。
func (v *Verifier) VerifyPackageSums(sigBytes, sums []byte, component, version, platform string, at time.Time) (string, error) {
	if v.Trust == nil {
		return "", newErr(KindOptions, "Verifier 未配置 Trust")
	}
	sf, err := ParseSig(sigBytes)
	if err != nil {
		return "", err
	}
	key, err := v.Trust.Lookup(sf.KeyID)
	if err != nil {
		return "", err
	}
	if key.Status != StatusActive && key.Status != StatusRetired {
		return "", newErr(KindKeyInvalid, "key_id %q 状态为 %s，不可用于验证", sf.KeyID, key.Status)
	}
	if !key.validAt(at) {
		return "", newErr(KindKeyInvalid, "key_id %q 不在有效期窗口内", sf.KeyID)
	}
	in, err := PackageSigInput(component, version, platform, sums)
	if err != nil {
		return "", err
	}
	if !ed25519.Verify(key.pub, in, sf.sig) {
		return "", newErr(KindSignature, "包内 SHA256SUMS 签名校验失败").with("key_id", sf.KeyID)
	}
	return sf.KeyID, nil
}

// PackageMeta 是 META/package.json。
type PackageMeta struct {
	Component           string `json:"component"`
	Version             string `json:"version"`
	Variant             string `json:"variant"`
	Platform            string `json:"platform"`
	BuiltAt             string `json:"built_at"`
	ConfigSchemaVersion int    `json:"config_schema_version"`
	FilesCount          int    `json:"files_count"`
	UnpackedSize        int64  `json:"unpacked_size"`
}

// ParsePackageMeta 解析并校验 META/package.json（未知字段忽略；必需字段缺失或非法拒绝）。
func ParsePackageMeta(b []byte) (*PackageMeta, error) {
	if len(b) == 0 || len(b) > 64<<10 {
		return nil, newErr(KindPackage, "META/package.json 大小非法")
	}
	if err := scanJSON(b); err != nil {
		return nil, &Error{Kind: KindPackage, Msg: "META/package.json 不是无歧义的 JSON", Cause: err}
	}
	var tree map[string]any
	if err := json.Unmarshal(b, &tree); err != nil {
		return nil, &Error{Kind: KindPackage, Msg: "META/package.json 不是 JSON 对象", Cause: err}
	}
	for _, f := range []string{"component", "version", "variant", "platform", "built_at", "config_schema_version", "files_count", "unpacked_size"} {
		if _, ok := tree[f]; !ok {
			return nil, newErr(KindPackage, "META/package.json 缺少必需字段 %q", f)
		}
	}
	var m PackageMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, &Error{Kind: KindPackage, Msg: "META/package.json 字段类型不符", Cause: err}
	}
	if !reComponent.MatchString(m.Component) || !reVariant.MatchString(m.Variant) || !rePlatform.MatchString(m.Platform) {
		return nil, newErr(KindPackage, "META/package.json 的 component/variant/platform 非法")
	}
	if _, err := ParseVersion(m.Version); err != nil {
		return nil, err
	}
	if _, err := time.Parse(time.RFC3339, m.BuiltAt); err != nil {
		return nil, newErr(KindPackage, "META/package.json 的 built_at 不是 RFC 3339")
	}
	if m.ConfigSchemaVersion < 0 || m.FilesCount < 1 || m.UnpackedSize < 0 {
		return nil, newErr(KindPackage, "META/package.json 的数值字段非法")
	}
	return &m, nil
}

// Matches 校验包 META 与清单选定条目一致（§9.5 第 7 步）。
func (m *PackageMeta) Matches(component, version, variant, platform string) error {
	pv, err := ParseVersion(version)
	if err != nil {
		return err
	}
	mv, err := ParseVersion(m.Version)
	if err != nil {
		return err
	}
	if m.Component != component || mv.Compare(pv) != 0 || m.Version != pv.String() || m.Variant != variant || m.Platform != platform {
		return newErr(KindPackage, "包 META（%s %s %s %s）与清单选定条目（%s %s %s %s）不一致",
			m.Component, m.Version, m.Variant, m.Platform, component, pv.String(), variant, platform)
	}
	return nil
}
