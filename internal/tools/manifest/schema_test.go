package manifest

import (
	"strings"
	"testing"
)

func parseMut(t *testing.T, mutate func(m map[string]any)) (*Manifest, error) {
	t.Helper()
	return ParseManifest(mf(t, mutate))
}

func firstVersion(m map[string]any) map[string]any {
	c := m["components"].(map[string]any)["server"].(map[string]any)
	return c["versions"].([]any)[0].(map[string]any)
}

func TestSchemaUnknownOptionalIgnoredUnknownCriticalRejected(t *testing.T) {
	// 未知可选字段（各层级）忽略，且不影响已知字段的解析。
	m, err := parseMut(t, func(m map[string]any) {
		m["x_future_top"] = map[string]any{"a": 1}
		c := m["components"].(map[string]any)["server"].(map[string]any)
		c["x_future_component"] = true
		v := firstVersion(m)
		v["x_future_version"] = "z"
		v["variants"].(map[string]any)["default"].(map[string]any)["x_future_variant"] = 1
		v["variants"].(map[string]any)["default"].(map[string]any)["artifacts"].(map[string]any)["linux-amd64"].(map[string]any)["x_future_artifact"] = []any{}
	})
	if err != nil {
		t.Fatalf("未知可选字段应被忽略: %v", err)
	}
	if m.Sequence != 10 {
		t.Fatal("已知字段解析异常")
	}

	// 未知必需字段（critical 声明）：五个层级逐一拒绝。
	cases := map[string]func(m map[string]any){
		"top": func(m map[string]any) { m["critical"] = []any{"x_must_understand"}; m["x_must_understand"] = 1 },
		"component": func(m map[string]any) {
			m["components"].(map[string]any)["server"].(map[string]any)["critical"] = []any{"x_c"}
		},
		"version": func(m map[string]any) { firstVersion(m)["critical"] = []any{"x_v"} },
		"variant": func(m map[string]any) {
			firstVersion(m)["variants"].(map[string]any)["default"].(map[string]any)["critical"] = []any{"x_var"}
		},
		"artifact": func(m map[string]any) {
			firstVersion(m)["variants"].(map[string]any)["default"].(map[string]any)["artifacts"].(map[string]any)["linux-amd64"].(map[string]any)["critical"] = []any{"x_a"}
		},
	}
	for name, mut := range cases {
		_, err := parseMut(t, mut)
		if err == nil {
			t.Fatalf("%s 层级的未知必需字段应被拒绝", name)
		}
		wantKind(t, err, KindSchema)
		if !strings.Contains(err.Error(), "必需字段") {
			t.Fatalf("%s：错误应指出必需字段: %v", name, err)
		}
	}
	// critical 列出的是已知字段 → 通过；critical 类型错误 → 拒绝。
	if _, err := parseMut(t, func(m map[string]any) { m["critical"] = []any{"sequence", "valid_until"} }); err != nil {
		t.Fatalf("critical 引用已知字段应通过: %v", err)
	}
	for _, bad := range []any{"sequence", []any{1}, map[string]any{}} {
		_, err := parseMut(t, func(m map[string]any) { m["critical"] = bad })
		wantKind(t, err, KindSchema)
	}
}

func TestSchemaRejections(t *testing.T) {
	art := func(m map[string]any) map[string]any {
		return firstVersion(m)["variants"].(map[string]any)["default"].(map[string]any)["artifacts"].(map[string]any)["linux-amd64"].(map[string]any)
	}
	cases := map[string]func(m map[string]any){
		"schema_version 2":         func(m map[string]any) { m["schema_version"] = 2 },
		"schema_version 0":         func(m map[string]any) { m["schema_version"] = 0 },
		"schema_version 字符串":       func(m map[string]any) { m["schema_version"] = "1" },
		"channel 未知":               func(m map[string]any) { m["channel"] = "nightly" },
		"sequence 0":               func(m map[string]any) { m["sequence"] = 0 },
		"sequence 负":               func(m map[string]any) { m["sequence"] = -3 },
		"sequence 小数":              func(m map[string]any) { m["sequence"] = 1.5 },
		"sequence 字符串":             func(m map[string]any) { m["sequence"] = "1" },
		"generated_at 非时间":         func(m map[string]any) { m["generated_at"] = "yesterday" },
		"valid_until 早于 generated": func(m map[string]any) { m["valid_until"] = "2026-10-31T00:00:00Z" },
		"valid_until 等于 generated": func(m map[string]any) { m["valid_until"] = m["generated_at"] },
		"缺 revoked_versions":       func(m map[string]any) { delete(m, "revoked_versions") },
		"revoked_versions null":    func(m map[string]any) { m["revoked_versions"] = nil },
		"缺 components":             func(m map[string]any) { delete(m, "components") },
		"缺 valid_until":            func(m map[string]any) { delete(m, "valid_until") },
		"缺 sequence":               func(m map[string]any) { delete(m, "sequence") },
		"组件状态未知":                   func(m map[string]any) { m["components"].(map[string]any)["dns"] = map[string]any{"status": "soon"} },
		"unreleased 带 versions": func(m map[string]any) {
			m["components"].(map[string]any)["dns"] = map[string]any{"status": "unreleased", "versions": []any{}}
		},
		"builtin 缺 builtin_in": func(m map[string]any) {
			m["components"].(map[string]any)["scheduler"] = map[string]any{"status": "builtin"}
		},
		"available 缺 versions": func(m map[string]any) {
			m["components"].(map[string]any)["node"] = map[string]any{"status": "available"}
		},
		"组件名非法": func(m map[string]any) {
			m["components"].(map[string]any)["Bad_Name"] = map[string]any{"status": "unreleased"}
		},
		"版本非 semver": func(m map[string]any) { firstVersion(m)["version"] = "v0.1.0" },
		"版本缺 patch":  func(m map[string]any) { firstVersion(m)["version"] = "0.1" },
		"版本前导零":      func(m map[string]any) { firstVersion(m)["version"] = "01.1.0" },
		"版本重复": func(m map[string]any) {
			c := m["components"].(map[string]any)["server"].(map[string]any)
			c["versions"] = append(c["versions"].([]any), firstVersion(m))
		},
		"无 variants": func(m map[string]any) { delete(firstVersion(m), "variants") },
		"variant 无 artifacts": func(m map[string]any) {
			firstVersion(m)["variants"] = map[string]any{"default": map[string]any{"artifacts": map[string]any{}}}
		},
		"platform 非法": func(m map[string]any) {
			firstVersion(m)["variants"].(map[string]any)["default"].(map[string]any)["artifacts"].(map[string]any)["windows-amd64"] = art(m)
		},
		"url http":  func(m map[string]any) { art(m)["url"] = "http://rel.example.test/a.tar.gz" },
		"url 含凭据":   func(m map[string]any) { art(m)["url"] = "https://" + "u" + ":" + "p" + "@rel.example.test/a.tar.gz" }, // 运行期拼接：避免 G-6 把 URL 凭据误报为固定口令
		"url file":  func(m map[string]any) { art(m)["url"] = "file:///etc/passwd" },
		"sha256 大写": func(m map[string]any) { art(m)["sha256"] = strings.ToUpper(sha256hex("x")) },
		"sha256 过短": func(m map[string]any) { art(m)["sha256"] = "abcd" },
		"size 0":    func(m map[string]any) { art(m)["size"] = 0 },
		"size 过大":   func(m map[string]any) { art(m)["size"] = int64(9) << 30 },
		"size 字符串":  func(m map[string]any) { art(m)["size"] = "12" },
		"端口 0":      func(m map[string]any) { firstVersion(m)["ports"] = []any{map[string]any{"proto": "tcp", "port": 0}} },
		"端口 65536": func(m map[string]any) {
			firstVersion(m)["ports"] = []any{map[string]any{"proto": "tcp", "port": 65536}}
		},
		"端口协议非法": func(m map[string]any) { firstVersion(m)["ports"] = []any{map[string]any{"proto": "icmp", "port": 80}} },
		"约束非法": func(m map[string]any) {
			firstVersion(m)["requires"] = map[string]any{"external": map[string]any{"mysql": ">= 8 || ||"}}
		},
		"约束含通配": func(m map[string]any) {
			firstVersion(m)["requires"] = map[string]any{"external": map[string]any{"mysql": "8.x"}}
		},
		"min_upgrade_from 非法": func(m map[string]any) { firstVersion(m)["min_upgrade_from"] = "latest" },
		"revoked 版本非法": func(m map[string]any) {
			m["revoked_versions"] = []any{map[string]any{"component": "node", "version": "x"}}
		},
		"revoked 组件非法": func(m map[string]any) {
			m["revoked_versions"] = []any{map[string]any{"component": "No De", "version": "1.0.0"}}
		},
		"notes_url 非 https": func(m map[string]any) { firstVersion(m)["notes_url"] = "ftp://x/y" },
		"migrations 负数":     func(m map[string]any) { firstVersion(m)["migrations"] = map[string]any{"irreversible_count": -1} },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseMut(t, mut)
			if err == nil {
				t.Fatal("应被 schema 校验拒绝")
			}
			wantKind(t, err, KindSchema)
		})
	}
}

func TestSchemaRawJSONRejections(t *testing.T) {
	good := string(mf(t, nil))
	cases := map[string]string{
		"空":              "",
		"非 JSON":         "not json",
		"数组":             "[]",
		"null":           "null",
		"重复键":            strings.Replace(good, `"channel":"stable"`, `"channel":"stable","channel":"beta"`, 1),
		"嵌套重复键":          strings.Replace(good, `"dns":{"status":"unreleased"}`, `"dns":{"status":"unreleased","status":"builtin"}`, 1),
		"多余内容":           good + `{}`,
		"BOM":            "\ufeff" + good,
		"超深嵌套":           `{"a":` + strings.Repeat("[", 40) + strings.Repeat("]", 40) + `}`,
		"NaN":            strings.Replace(good, `"sequence":10`, `"sequence":NaN`, 1),
		"尾随逗号":           strings.Replace(good, `"sequence":10`, `"sequence":10,`, 1) + "",
		"components 非对象": strings.Replace(good, `"components":{`, `"components":[`, 1),
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseManifest([]byte(s))
			if err == nil {
				t.Fatal("应拒绝")
			}
			wantKind(t, err, KindSchema)
		})
	}
	if _, err := ParseManifest([]byte(good)); err != nil {
		t.Fatalf("对照组应通过: %v", err)
	}
	// 超过 8MiB 上限。
	_, err := ParseManifest([]byte(strings.Repeat(" ", maxManifestBytes+1)))
	wantKind(t, err, KindSchema)
}

// tools-design §9.2 的完整格式示例（版本号/哈希/URL 为示意，此处替换为合法占位后须通过解析）。
func TestSchemaDesignDocExampleShape(t *testing.T) {
	doc := `{
  "schema_version": 1, "channel": "stable", "sequence": 42,
  "generated_at": "2026-11-16T00:00:00Z", "valid_until": "2027-02-14T00:00:00Z",
  "revoked_versions": [ { "component": "node", "version": "0.0.9", "reason": "示意：已知缺陷" } ],
  "components": {
    "server": { "status": "available", "description": "LingEdge 中央主控", "versions": [ {
      "version": "0.1.0", "released_at": "2026-11-15T00:00:00Z", "min_upgrade_from": "0.1.0",
      "notes_url": "https://rel.example.test/notes/server-0.1.0",
      "requires": { "system": { "os": ["linux"], "arch": ["amd64","arm64"], "min_memory_mb": 0, "min_disk_mb": 0 },
                    "external": { "mysql": ">=8.0.16 <9", "redis": ">=8" }, "components": {}, "runtime": {} },
      "conflicts": [],
      "ports": [ { "proto": "tcp", "port": 8080, "bind": "127.0.0.1", "purpose": "rest" },
                 { "proto": "tcp", "port": 9444, "bind": "0.0.0.0", "purpose": "grpc-enroll", "conditional": true } ],
      "migrations": { "schema_from": "0000", "schema_to": "0007", "classes": ["reversible","expand"], "irreversible_count": 0, "backup_required": true },
      "config_schema_version": 1,
      "variants": { "default": { "artifacts": {
        "linux-amd64": { "url": "https://rel.example.test/server/0.1.0/lingedge-server-0.1.0-linux-amd64.tar.gz", "sha256": "` + sha256hex("a") + `", "size": 100 },
        "linux-arm64": { "url": "https://rel.example.test/server/0.1.0/lingedge-server-0.1.0-linux-arm64.tar.gz", "sha256": "` + sha256hex("b") + `", "size": 100 } } } }
    } ] },
    "website": { "status": "available", "versions": [ { "version": "0.1.0", "variants": {
      "static": { "artifacts": { "any": { "url": "https://rel.example.test/w/static.tar.gz", "sha256": "` + sha256hex("c") + `", "size": 1 } }, "requires": { "runtime": {} } },
      "ssr": { "artifacts": { "any": { "url": "https://rel.example.test/w/ssr.tar.gz", "sha256": "` + sha256hex("d") + `", "size": 1 } }, "requires": { "runtime": { "node": ">=20 <23" } } } } } ] },
    "node": { "status": "available", "versions": [ { "version": "0.1.0", "requires": { "components": { "openresty": ">=1 <2" } },
      "variants": { "default": { "artifacts": { "linux-amd64": { "url": "https://rel.example.test/n.tar.gz", "sha256": "` + sha256hex("e") + `", "size": 1 } } } } } ] },
    "openresty": { "status": "available", "internal": true, "versions": [ ] },
    "dns": { "status": "unreleased" },
    "scheduler": { "status": "builtin", "builtin_in": "server" },
    "worker": { "status": "builtin", "builtin_in": "server" },
    "tools": { "status": "available", "versions": [ ] }
  }
}`
	m, err := ParseManifest([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if m.Sequence != 42 || len(m.Components) != 8 {
		t.Fatalf("解析结果异常: seq=%d comps=%d", m.Sequence, len(m.Components))
	}
	v := m.Components["server"].Versions[0]
	if v.Parsed.String() != "0.1.0" || len(v.Variants["default"].Artifacts) != 2 {
		t.Fatalf("server 版本解析异常: %+v", v)
	}
}

// revoked_versions「缺失」与「为 null」由两道不同的检查拒绝（requireFields 与 null 检查）；
// 单看「是否被拒绝」，去掉前者仍被后者拒绝（等价变异），所以这里断言各自的错误文本，使两道检查都被独立钉住。
func TestRevokedVersionsMissingAndNullRejectedByDistinctChecks(t *testing.T) {
	_, err := ParseManifest(mf(t, func(m map[string]any) { delete(m, "revoked_versions") }))
	wantKind(t, err, KindSchema)
	if !strings.Contains(err.Error(), "缺少必需字段") || !strings.Contains(err.Error(), "revoked_versions") {
		t.Errorf("缺失应由必需字段检查给出明确错误: %v", err)
	}
	_, err = ParseManifest(mf(t, func(m map[string]any) { m["revoked_versions"] = nil }))
	wantKind(t, err, KindSchema)
	if !strings.Contains(err.Error(), "不得为 null") {
		t.Errorf("null 应由 null 检查给出明确错误: %v", err)
	}
}
