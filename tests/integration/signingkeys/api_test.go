package signingkeysit

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/leakguard"
	"github.com/wuekevin/lingedge/internal/platform/signingkeys"
	"github.com/wuekevin/lingedge/pkg/secretbox"
	"github.com/wuekevin/lingedge/pkg/signing"
	"github.com/wuekevin/lingedge/tests/apistack"
)

// AC4：GET /admin/system/signing-keys 只返回公钥与指纹，绝不含私钥；契约一致；授权矩阵。
func TestListSigningKeysAPI(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)

	// 初始化之前：空列表（不是 501/404）。
	r := adm.Do("GET", "/admin/system/signing-keys", nil)
	w.Contract("listSigningKeys", "GET", "/admin/system/signing-keys", r)
	if m := r.JSON(t); r.Status != 200 || len(m["items"].([]any)) != 0 || m["total"] != float64(0) {
		t.Fatalf("初始化前应为空列表: %d %s", r.Status, r.Body)
	}

	res, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL})
	if err != nil {
		t.Fatal(err)
	}
	keys := readKeys(t, w)

	r = adm.Do("GET", "/admin/system/signing-keys", nil)
	w.Contract("listSigningKeys", "GET", "/admin/system/signing-keys", r)
	if r.Status != 200 {
		t.Fatalf("状态 %d: %s", r.Status, r.Body)
	}
	m := r.JSON(t)
	items := m["items"].([]any)
	if len(items) != 3 || m["total"] != float64(3) {
		t.Fatalf("应有 3 条: %s", r.Body)
	}
	allowed := map[string]bool{"key_id": true, "purpose": true, "algorithm": true, "status": true, "public_key": true, "fingerprint": true,
		"not_after": true, "activated_at": true, "retire_after": true, "revoked_at": true}
	byP := map[string]map[string]any{}
	for _, it := range items {
		o := it.(map[string]any)
		for k := range o {
			if !allowed[k] {
				t.Errorf("响应含白名单之外的字段 %q", k)
			}
		}
		byP[o["purpose"].(string)] = o
	}
	for _, k := range keys {
		o := byP[k.Purpose]
		if o == nil || o["key_id"] != k.KeyID || o["status"] != "active" || o["algorithm"] != k.Algorithm {
			t.Errorf("%s 条目不符: %v", k.Purpose, o)
			continue
		}
		pub, err := base64.StdEncoding.DecodeString(o["public_key"].(string))
		if err != nil || !bytes.Equal(pub, k.Public) {
			t.Errorf("%s 公钥不符", k.Purpose)
		}
	}
	// 指纹与 Initialize 返回、pkg/signing 的口径一致；证书类带 not_after，配置签名没有。
	for _, ki := range res.Keys {
		if byP[string(ki.Purpose)]["fingerprint"] != ki.Fingerprint {
			t.Errorf("%s 指纹不符", ki.Purpose)
		}
	}
	if byP["config_signing"]["fingerprint"] != signing.Fingerprint(byPurpose(t, keys, "config_signing").Public) || byP["config_signing"]["not_after"] != nil {
		t.Errorf("配置签名条目: %v", byP["config_signing"])
	}
	if byP["mtls_ca"]["not_after"] == nil || byP["grpc_server"]["not_after"] == nil {
		t.Error("证书类条目应带 not_after")
	}
	// 分页。
	p2 := adm.Do("GET", "/admin/system/signing-keys?page=2&page_size=2", nil)
	w.Contract("listSigningKeys", "GET", "/admin/system/signing-keys?page=2&page_size=2", p2)
	if n := len(p2.JSON(t)["items"].([]any)); n != 1 {
		t.Errorf("第二页应 1 条: %s", p2.Body)
	}

	// 私钥不外泄：响应体不含私钥明文的任何常见编码、不含 PEM 私钥头/密文列内容；leakguard 无违规。
	assertNoPrivateMaterial(t, w, "API 响应", r.Body, p2.Body)
	if v := leakguard.Scan(r.Body, leakguard.Options{Internal: w.SeedInternals().Strings()}); len(v) > 0 {
		t.Errorf("leakguard 违规: %v", v)
	}
	for _, k := range keys {
		if strings.Contains(string(r.Body), k.Enc) || strings.Contains(string(r.Body), "private") || strings.Contains(string(r.Body), "PRIVATE") {
			t.Errorf("响应不得含密文列或 private 字样")
		}
	}

	// 授权：system:read；未登录 401；无权限平台用户 403；租户 Console 会话 401（platform 路由）；租户令牌 403；有 system:read 的平台令牌 200。
	if r := w.Do("GET", "/admin/system/signing-keys", nil); r.Status != 401 {
		t.Errorf("未登录应 401: %d", r.Status)
	}
	nobody := w.Login(w.SeedUser("platform", nil))
	if r := nobody.Do("GET", "/admin/system/signing-keys", nil); r.Status != 403 {
		t.Errorf("无 system:read 应 403: %d", r.Status)
	}
	tn := w.SeedTenant("sk")
	tu := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(tu, "tenant_owner")
	ts := w.Login(tu)
	if r := ts.Do("GET", "/admin/system/signing-keys", nil); r.Status != 401 {
		t.Errorf("租户 Console 会话访问平台路由应 401: %d", r.Status)
	}
	tok := ts.IssueToken("sk-probe", "task:read")
	if r := w.Bearer(tok, "GET", "/admin/system/signing-keys", nil); r.Status != 403 {
		t.Errorf("租户令牌应 403: %d", r.Status)
	}
	ptok := adm.IssueToken("sk-platform", "system:read")
	if r := w.Bearer(ptok, "GET", "/admin/system/signing-keys", nil); r.Status != 200 {
		t.Errorf("有 system:read 的平台令牌应 200: %d %s", r.Status, r.Body)
	}
	// 租户 acting-as 头不能让租户主体读取（平台路由对租户主体 fail-closed，且 platform 路由不接受 X-Tenant-ID 之外的放宽）。
	if r := w.Bearer(tok, "GET", "/admin/system/signing-keys", nil, apistack.ActingAs(tn.UID)); r.Status != 403 {
		t.Errorf("租户令牌带 X-Tenant-ID 仍应 403: %d", r.Status)
	}
	// 只读：写方法不存在（405/404，不是 501 占位）。
	for _, meth := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if r := adm.Do(meth, "/admin/system/signing-keys", map[string]any{}); r.Status < 400 || r.Status == 501 {
			t.Errorf("%s 不应被支持: %d", meth, r.Status)
		}
	}
}

// 私钥不外泄断言：初始化 + 签名 + 读取 API 之后，真实私钥明文（PKCS#8 DER、Ed25519 种子/私钥、ECDSA 标量；原始、hex、各 base64 编码）
// 不出现在日志、审计表、任何 API 响应、也不出现在数据库任何表的任何单元格里（密文列是信封密文，不含明文）。
func TestPrivateKeysNeverLeak(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	if _, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := w.Keys.Sign(ctxT(t), []byte(fmt.Sprintf(`{"seq":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	var bodies [][]byte
	for _, p := range []string{"/admin/system/signing-keys", "/admin/settings", "/admin/system/version", "/audit-logs?limit=100", "/admin/installations"} {
		r := adm.Do("GET", p, nil)
		bodies = append(bodies, r.Body)
	}
	assertNoPrivateMaterial(t, w, "全部响应、日志与全库", bodies...)

	// 审计摘要里只有公开指纹：逐行扫描 changes_json/meta_json，且结构上含 fingerprints。
	rows := w.RootStr("SELECT CAST(changes_json AS CHAR) FROM §.audit_logs WHERE action = 'system.signing_keys.init'")
	var ch map[string]any
	if err := json.Unmarshal([]byte(rows), &ch); err != nil || ch["fingerprints"] == nil || ch["lineage_initialized"] != true {
		t.Fatalf("审计摘要: %s %v", rows, err)
	}
	if strings.Contains(strings.ToLower(rows), "private") {
		t.Errorf("审计摘要不应含 private 字样: %s", rows)
	}
}

// assertNoPrivateMaterial 取库内全部私钥的明文（经真实信封加密解出）及其各种编码作为「针」，在 extra（响应体）、服务端日志、全库全部单元格里搜索。
// 密文列自身（信封密文）会被一并扫描——它不得含明文；同时断言扫描确有效：针必须在「解密后的明文」里命中（正向自检），且全库确有内容。
func assertNoPrivateMaterial(t *testing.T, w *apistack.World, what string, extra ...[]byte) {
	t.Helper()
	keys := readKeys(t, w)
	if len(keys) == 0 {
		t.Fatal("没有密钥，扫描无意义")
	}
	var all [][]byte
	for _, k := range keys {
		der := decrypt(t, w, k)
		ns := needles(t, der)
		secretbox.Zero(der)
		all = append(all, ns...)
	}
	if len(all) < 20 {
		t.Fatalf("针的数量异常: %d", len(all))
	}
	hay := [][]byte{[]byte(w.Logs.String())}
	hay = append(hay, extra...)
	hay = append(hay, dumpDB(t, w)...)
	for _, k := range keys { // 同时扫描 PEM 形态的私钥头
		hay = append(hay, []byte(k.Enc))
	}
	for _, n := range all {
		for i, h := range hay {
			if bytes.Contains(h, n) {
				t.Fatalf("%s：私钥材料（%d 字节的针）出现在第 %d 份内容中（日志/响应/库）", what, len(n), i)
			}
		}
	}
	for _, h := range hay {
		if bytes.Contains(h, []byte("PRIVATE KEY-----")) {
			t.Fatalf("%s：出现 PEM 私钥块", what)
		}
	}
	// 正向自检：同一套针能在明文里命中——证明扫描逻辑有效（不是恒不命中）。
	der := decrypt(t, w, keys[0])
	defer secretbox.Zero(der)
	n0 := needles(t, der)
	if !bytes.Contains(append([]byte("x"), der...), n0[0]) || !bytes.Contains([]byte("a"+base64.StdEncoding.EncodeToString(der)+"b"), n0[3]) {
		t.Fatal("扫描自检失败：针无法在明文中命中")
	}
}

// 设置项 grpc.tls.san_extra（Story 1.23 新增键）：经 PATCH /admin/settings 修改需再认证与审计，取值受校验；初始化签发的叶证书 SAN 采用它。
func TestSANExtraSettingFlowsIntoLeafCert(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	patch := func(v any) apistack.Resp {
		return adm.Do("PATCH", "/admin/settings", map[string]any{"values": map[string]any{"grpc.tls.san_extra": v}})
	}
	if r := patch([]string{"edge2.example.test"}); r.Status != 403 || r.ErrCode(t) != "auth.reauth_required" {
		t.Fatalf("未再认证应 403 auth.reauth_required: %d %s", r.Status, r.Body)
	}
	adm.Reauth()
	for _, bad := range []any{"edge2.example.test", []string{"bad host"}, []string{"a.test:9443"}, []int{1}} {
		if r := patch(bad); r.Status != 400 {
			t.Errorf("非法取值 %v 应 400: %d %s", bad, r.Status, r.Body)
		}
	}
	if r := patch([]string{"Edge2.Example.Test", "10.20.30.40"}); r.Status != 200 {
		t.Fatalf("合法取值应 200: %d %s", r.Status, r.Body)
	}
	if n := w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'setting.update'"); n != 1 {
		t.Errorf("修改应写一条审计: %d", n)
	}
	if _, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL}); err != nil {
		t.Fatal(err)
	}
	leaf := cert(t, byPurpose(t, readKeys(t, w), "grpc_server"))
	if got := strings.Join(leaf.DNSNames, ","); got != "edge.example.test,edge2.example.test" {
		t.Errorf("DNS SAN: %s", got)
	}
	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "10.20.30.40" {
		t.Errorf("IP SAN: %v", leaf.IPAddresses)
	}
	// 初始化之后再改 san_extra 不会改动已签发的叶证书（热重载/轮换属 3.10）。
	if r := patch([]string{"other.example.test"}); r.Status != 200 {
		t.Fatal(r.Body)
	}
	if _, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cert(t, byPurpose(t, readKeys(t, w), "grpc_server")).DNSNames, ","); got != "edge.example.test,edge2.example.test" {
		t.Errorf("既有叶证书不应被改动: %s", got)
	}
}
