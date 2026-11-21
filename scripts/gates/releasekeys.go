package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// release-keys 门禁（Story 1.27 AC5、NFR-1、ADR-012）：内置发行公钥集 internal/tools/manifest/builtin/keys.json 由发行流水线在构建前注入（不入库）。
//
//	缺失        默认（开发/CI 构建）放行——此时 lingedge 内置零把公钥，运行期对任何清单失败关闭（no_builtin_keys）；
//	            -require（make build RELEASE=1）下缺失即构建失败（release-keys-missing）。
//	含测试公钥  任何模式下失败：公钥与 data/test-release-pubkeys.txt 的指纹相同（公钥字节比较，与编码无关），或 key_id 带 test/golden/demo 等字样；
//	格式非法    失败：schema_version、alg=ed25519、32 字节公钥、status、key_id 格式、至少一把 active 且未吊销的公钥、文件中不得出现私钥/种子字段；
//	杂散文件    builtin/ 下除 README.txt 与 keys.json 外的任何文件都会被 go:embed 一并打进产物，一律失败（防止测试公钥换个文件名混入）。
//
// 门禁自身的正/负向样本在 releasekeys_test.go（make gate-selftest）与 tests/security/releasekeys（对真实 go build 的二进制扫描）。

const (
	releaseKeysDir  = "internal/tools/manifest/builtin"
	releaseKeysFile = releaseKeysDir + "/keys.json"
)

var reTestKeyID = regexp.MustCompile(`(?i)(^|[-_.])(test|testing|golden|demo|fake|mock|dummy|sample|example)([-_.]|$)`)

// testReleasePubKeys 返回测试公钥指纹表（32 字节公钥，以字符串为键）。
func testReleasePubKeys() map[string]string {
	out := map[string]string{}
	for _, l := range loadLines("test-release-pubkeys.txt") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(f[0])
		if err != nil || len(raw) != ed25519.PublicKeySize {
			panic("data/test-release-pubkeys.txt 含非法公钥行: " + l)
		}
		out[string(raw)] = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(l, f[0]), " #"))
	}
	return out
}

// checkReleaseKeys 检查 root 下的 builtin/ 目录；require 表示这是发行构建。
func checkReleaseKeys(root string, require bool) []Finding {
	var out []Finding
	add := func(file, rule, f string, a ...any) {
		out = append(out, Finding{File: file, Rule: rule, Msg: fmt.Sprintf(f, a...)})
	}
	dir := filepath.Join(root, filepath.FromSlash(releaseKeysDir))
	ents, err := os.ReadDir(dir)
	if err != nil {
		add(releaseKeysDir, "release-keys-dir", "无法读取内置公钥目录: %v（该目录由 go:embed 引用，必须存在）", err)
		return out
	}
	testKeys := testReleasePubKeys()
	hasKeys := false
	for _, e := range ents {
		rel := releaseKeysDir + "/" + e.Name()
		switch {
		case e.Name() == "README.txt":
			continue
		case e.Name() != "keys.json":
			add(rel, "release-keys-stray-file", "builtin/ 下只允许 README.txt 与 keys.json：其余文件会被 go:embed 打进产物（NFR-1）")
			continue
		}
		hasKeys = true
		fi, err := os.Lstat(filepath.Join(dir, e.Name()))
		if err != nil || !fi.Mode().IsRegular() {
			add(rel, "release-keys-format", "keys.json 必须是普通文件")
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			add(rel, "release-keys-format", "无法读取: %v", err)
			continue
		}
		out = append(out, checkKeysJSON(rel, data, testKeys)...)
	}
	// 任何文件（含 README.txt）里出现测试公钥的 base64/hex 表示都视为污染。
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		low := strings.ToLower(string(b))
		for raw, why := range testKeys {
			if strings.Contains(string(b), base64.StdEncoding.EncodeToString([]byte(raw))) || strings.Contains(low, hex.EncodeToString([]byte(raw))) {
				add(releaseKeysDir+"/"+e.Name(), "release-keys-test-pubkey", "出现测试公钥（%s）：测试密钥只允许在 tests/、_test.go 与 lingedge_testdouble 标签内（NFR-1）", why)
			}
		}
	}
	if !hasKeys && require {
		add(releaseKeysFile, "release-keys-missing", "发行构建缺少内置公钥 keys.json：由发行流水线注入真实公钥（UD-3 待发行签名人确认）；缺失不得构建发行产物（NFR-1）")
	}
	return out
}

func checkKeysJSON(rel string, data []byte, testKeys map[string]string) []Finding {
	var out []Finding
	add := func(rule, f string, a ...any) {
		out = append(out, Finding{File: rel, Rule: rule, Msg: fmt.Sprintf(f, a...)})
	}
	var doc struct {
		SchemaVersion int               `json:"schema_version"`
		Keys          []json.RawMessage `json:"keys"`
		Revoked       []string          `json:"revoked_key_ids"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		add("release-keys-format", "keys.json 不是合法 JSON: %v", err)
		return out
	}
	if doc.SchemaVersion != 1 {
		add("release-keys-format", "schema_version 必须为 1")
	}
	revoked := map[string]bool{}
	for _, id := range doc.Revoked {
		revoked[id] = true
	}
	active := 0
	for i, rk := range doc.Keys {
		var generic map[string]any
		_ = json.Unmarshal(rk, &generic)
		for k := range generic {
			switch strings.ToLower(k) {
			case "private_key", "privatekey", "seed", "secret", "secret_key", "priv", "private":
				add("release-keys-private-material", "keys[%d] 含私钥/种子字段 %q：发行私钥离线保管，内置文件只能有公钥", i, k)
			}
		}
		var k struct {
			KeyID     string `json:"key_id"`
			Alg       string `json:"alg"`
			PublicKey string `json:"public_key"`
			Status    string `json:"status"`
		}
		if err := json.Unmarshal(rk, &k); err != nil {
			add("release-keys-format", "keys[%d] 格式非法: %v", i, err)
			continue
		}
		if k.Alg != "ed25519" {
			add("release-keys-format", "keys[%d]（%s）alg 必须是 ed25519", i, k.KeyID)
		}
		raw, err := base64.StdEncoding.Strict().DecodeString(k.PublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			add("release-keys-format", "keys[%d]（%s）public_key 必须是 32 字节标准 base64", i, k.KeyID)
		} else if why, bad := testKeys[string(raw)]; bad {
			add("release-keys-test-pubkey", "keys[%d]（%s）是测试公钥（%s）：生产构建不得内置测试公钥（NFR-1）", i, k.KeyID, why)
		}
		if reTestKeyID.MatchString(k.KeyID) {
			add("release-keys-test-key-id", "keys[%d] 的 key_id %q 含 test/golden/demo 等字样：疑似测试密钥", i, k.KeyID)
		}
		switch k.Status {
		case "active":
			if !revoked[k.KeyID] {
				active++
			}
		case "retired", "revoked":
		default:
			add("release-keys-format", "keys[%d]（%s）status 必须是 active|retired|revoked", i, k.KeyID)
		}
	}
	if active == 0 {
		add("release-keys-format", "keys.json 至少需要一把 active 且未被吊销的公钥")
	}
	return out
}

func runReleaseKeys(root string, opts options) ([]Finding, error) {
	if _, err := os.Stat(filepath.Join(root, "internal/tools/manifest")); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("root %s 下没有 internal/tools/manifest，不是 LingEdge 仓库根", root)
	}
	return checkReleaseKeys(root, opts.requireKeys), nil
}
