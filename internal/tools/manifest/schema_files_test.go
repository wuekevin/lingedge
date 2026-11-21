package manifest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// inlineRefs 把同文档内的 "#/$defs/<name>" 引用内联展开（kin-openapi 的 Schema 不解析 $defs；这些 schema 没有递归引用）。
func inlineRefs(node any, defs map[string]any) any {
	switch n := node.(type) {
	case map[string]any:
		if ref, ok := n["$ref"].(string); ok && strings.HasPrefix(ref, "#/$defs/") {
			return inlineRefs(defs[strings.TrimPrefix(ref, "#/$defs/")], defs)
		}
		out := map[string]any{}
		for k, v := range n {
			if k == "$defs" {
				continue
			}
			out[k] = inlineRefs(v, defs)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, v := range n {
			out[i] = inlineRefs(v, defs)
		}
		return out
	}
	return node
}

func loadSchema(t *testing.T, name string) *openapi3.Schema {
	t.Helper()
	b, err := SchemaFS.ReadFile("schema/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	defs, _ := doc["$defs"].(map[string]any)
	if name == "manifest.schema.json" { // 跨文件引用 keys.schema.json#/$defs/key：并入本文件的 defs
		kb, _ := SchemaFS.ReadFile("schema/keys.schema.json")
		var kd map[string]any
		_ = json.Unmarshal(kb, &kd)
		kdefs := kd["$defs"].(map[string]any)
		defs["key"] = inlineRefs(kdefs["key"], kdefs)
		props := doc["properties"].(map[string]any)["key_rotation"].(map[string]any)["properties"].(map[string]any)
		props["keys"].(map[string]any)["items"] = map[string]any{"$ref": "#/$defs/key"}
	}
	nb, _ := json.Marshal(inlineRefs(doc, defs))
	var s openapi3.Schema
	if err := json.Unmarshal(nb, &s); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return &s
}

func anyOf(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// 真实向量（golden、生成的清单、签名文件、keys 文档、包 META）都通过各自的 schema；schema 的必需字段缺失/非法值被拒绝。
func TestSchemasAcceptRealDocumentsAndRejectBad(t *testing.T) {
	ms := loadSchema(t, "manifest.schema.json")
	g := loadGolden(t)
	if err := ms.VisitJSON(anyOf(t, []byte(g.Manifest.FileText))); err != nil {
		t.Fatalf("golden 清单应通过 schema: %v", err)
	}
	e := newEnv(t)
	good := mf(t, nil)
	if err := ms.VisitJSON(anyOf(t, good)); err != nil {
		t.Fatalf("生成的清单应通过 schema: %v", err)
	}
	// Go 实现与 schema 一致：每个 schema 拒绝的变体，Go 也必须拒绝（反之 Go 拒绝的 schema 不保证拒绝，如重复键/约束语法）。
	bads := map[string]func(m map[string]any){
		"缺 sequence":     func(m map[string]any) { delete(m, "sequence") },
		"channel":        func(m map[string]any) { m["channel"] = "x" },
		"sequence 0":     func(m map[string]any) { m["sequence"] = 0 },
		"schema_version": func(m map[string]any) { m["schema_version"] = 2 },
		"组件状态":           func(m map[string]any) { m["components"].(map[string]any)["dns"] = map[string]any{"status": "soon"} },
		"sha256 大写": func(m map[string]any) {
			firstVersion(m)["variants"].(map[string]any)["default"].(map[string]any)["artifacts"].(map[string]any)["linux-amd64"].(map[string]any)["sha256"] = strings.ToUpper(sha256hex("x"))
		},
		"url http": func(m map[string]any) {
			firstVersion(m)["variants"].(map[string]any)["default"].(map[string]any)["artifacts"].(map[string]any)["linux-amd64"].(map[string]any)["url"] = "http://x/y"
		},
		"端口": func(m map[string]any) {
			firstVersion(m)["ports"] = []any{map[string]any{"proto": "tcp", "port": 70000}}
		},
		"版本": func(m map[string]any) { firstVersion(m)["version"] = "1.0" },
	}
	for name, mut := range bads {
		raw := mf(t, mut)
		if err := ms.VisitJSON(anyOf(t, raw)); err == nil {
			t.Errorf("%s：schema 应拒绝", name)
		}
		if _, err := ParseManifest(raw); err == nil {
			t.Errorf("%s：Go 实现应拒绝", name)
		}
	}
	// 未知可选字段 schema 允许（与 Go 一致）。
	if err := ms.VisitJSON(anyOf(t, mf(t, func(m map[string]any) { m["x_future"] = 1 }))); err != nil {
		t.Errorf("未知可选字段 schema 应允许: %v", err)
	}

	// keys / sig / package-meta。
	ks := loadSchema(t, "keys.schema.json")
	kd := keysDoc(t, []string{"rel-test-x"}, e.active.entry(StatusActive, "2026-01-01T00:00:00Z", "2028-01-01T00:00:00Z"))
	if err := ks.VisitJSON(anyOf(t, kd)); err != nil {
		t.Fatalf("keys 文档应通过: %v", err)
	}
	var kdm map[string]any
	_ = json.Unmarshal(kd, &kdm)
	kdm["extra"] = 1
	if err := ks.VisitJSON(kdm); err == nil {
		t.Error("keys schema 应拒绝未知字段")
	}
	sg := loadSchema(t, "sig.schema.json")
	if err := sg.VisitJSON(anyOf(t, signManifest(e.active, good))); err != nil {
		t.Fatalf("签名文件应通过: %v", err)
	}
	for _, bad := range []string{`{"key_id":"a","alg":"rsa","sig":"` + base64.StdEncoding.EncodeToString(make([]byte, 64)) + `"}`, `{"key_id":"a","alg":"ed25519","sig":"AAAA"}`, `{"key_id":"a","alg":"ed25519","sig":"` + base64.StdEncoding.EncodeToString(make([]byte, 64)) + `","x":1}`} {
		if err := sg.VisitJSON(anyOf(t, []byte(bad))); err == nil {
			t.Errorf("sig schema 应拒绝 %s", bad)
		}
	}
	ps := loadSchema(t, "package-meta.schema.json")
	meta := map[string]any{"component": "node", "version": "0.1.0", "variant": "default", "platform": "linux-amd64", "built_at": "2026-11-15T00:00:00Z", "config_schema_version": 1, "files_count": 3, "unpacked_size": 10}
	if err := ps.VisitJSON(meta); err != nil {
		t.Fatalf("package meta 应通过: %v", err)
	}
	meta["platform"] = "plan9"
	if err := ps.VisitJSON(meta); err == nil {
		t.Error("package-meta schema 应拒绝非法 platform")
	}
}

// CF-6 冻结：schema 文件内容的 SHA-256 钉在测试里。任何变更必须同时更新这里与 docs/bmad/contract-changes/CF-6.md 并新增变更说明。
var frozenSchemaHashes = map[string]string{
	"keys.schema.json":         "e54360f47a158bad93e8737b1be1568844bc8364406547fbfbc325a775134c75",
	"manifest.schema.json":     "5a4b3890ddd0405d3ce2a6310f161a11c895cef4a3b332cffd79ad8541d45cce",
	"package-meta.schema.json": "daf37c58e9559e4151df2d3a4e173d3d1953b3982db94acf68629c0380fdce11",
	"sig.schema.json":          "07334c91c2d35e77d5f8a0c598c63f238de9b2ca34b3d91a0d4a481add7dcd2c",
}

func TestSchemaFilesFrozen(t *testing.T) {
	ents, err := fs.ReadDir(SchemaFS, "schema")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range ents {
		b, _ := SchemaFS.ReadFile("schema/" + e.Name())
		h := sha256.Sum256(b)
		got[e.Name()] = hex.EncodeToString(h[:])
	}
	if len(got) != len(frozenSchemaHashes) {
		t.Fatalf("schema 文件集合与冻结清单不符：现有 %d 个，冻结 %d 个", len(got), len(frozenSchemaHashes))
	}
	for n, h := range frozenSchemaHashes {
		if got[n] != h {
			t.Errorf("CF-6 冻结的 %s 被改动：sha256=%s（冻结值 %s）。契约变更须走变更说明流程（docs/bmad/contract-changes/CF-6.md）", n, got[n], h)
		}
	}
}
