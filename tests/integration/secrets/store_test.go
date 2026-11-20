package secretsintegration

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/secrets"
	secretsrepo "github.com/wuekevin/lingedge/internal/platform/secrets/repository"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/pkg/secretbox"
)

func (e *env) store(t testing.TB) secretbox.RecordStore {
	t.Helper()
	repo, err := secretsrepo.New(e.app)
	if err != nil {
		t.Fatal(err)
	}
	s, err := secrets.NewRecordStore(repo, tenantscope.PlatformForJob(0))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// 白名单必须与 migrations 实际存在的 *_enc 列逐一对应：多出（声明了不存在的列）或遗漏（新迁移加了 *_enc 列而未登记）都失败；
// enc_key_id 列的有无也要与声明一致。
func TestTargetsMatchMigrations(t *testing.T) {
	e := newEnv(t)
	rows, err := e.adm.QueryContext(ctxT(t), "SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND COLUMN_NAME LIKE '%\\_enc' ORDER BY TABLE_NAME, COLUMN_NAME", e.dbName)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var actual []string
	for rows.Next() {
		var tb, c string
		if err := rows.Scan(&tb, &c); err != nil {
			t.Fatal(err)
		}
		actual = append(actual, tb+"."+c)
	}
	var declared []string
	for _, tc := range secretsrepo.Targets(tenantscope.PlatformForJob(0)) {
		declared = append(declared, tc[0]+"."+tc[1])
	}
	sort.Strings(declared)
	if strings.Join(actual, ",") != strings.Join(declared, ",") {
		t.Fatalf("白名单与 migrations 中实际的 *_enc 列不一致\n库内: %v\n声明: %v", actual, declared)
	}
	for _, tc := range secretsrepo.Targets(tenantscope.PlatformForJob(0)) {
		has := e.rootStr(t, "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = 'enc_key_id'", e.dbName, tc[0]) == "1"
		want := tc[0] == "certificates" || tc[0] == "signing_keys"
		if has != want {
			t.Errorf("%s 的 enc_key_id 列存在性 = %v，声明假设 %v", tc[0], has, want)
		}
	}
}

// 权限边界：四张目标表对 lingedge_app 有 UPDATE（本包的 CAS 写回依赖它）；构造器拒绝非 app 池与非平台 Scope。
func TestConstructorAndPrivilegeBoundary(t *testing.T) {
	e := newEnv(t)
	repo, err := secretsrepo.New(e.app)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.NewRecordStore(repo, tenantscope.Scope{}); !errors.Is(err, secrets.ErrNeedPlatformScope) {
		t.Errorf("零值 Scope 应被拒绝: %v", err)
	}
	ts, err := tenantscope.FromTask(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.NewRecordStore(repo, ts); !errors.Is(err, secrets.ErrNeedPlatformScope) {
		t.Errorf("租户范围 Scope 应被拒绝: %v", err)
	}
	// 直接调用仓储方法时同样逐次校验 Scope。
	if _, err := repo.CountByKEKAcrossTenants(ctxT(t), ts, "k1"); !errors.Is(err, secretsrepo.ErrNeedPlatformScope) {
		t.Errorf("仓储方法应拒绝租户范围 Scope: %v", err)
	}
	if _, err := secretsrepo.New(e.maint); !errors.Is(err, secretsrepo.ErrWrongRole) {
		t.Errorf("maint 池应被拒绝: %v", err)
	}
	// 真实授权核对：app 对四张表均有 SELECT/UPDATE；maint 对它们没有任何权限（即使类型检查被绕过，库也会拒绝）。
	grants := e.rootStr(t, "SELECT GROUP_CONCAT(CONCAT(TABLE_NAME, ':', PRIVILEGE_TYPE) ORDER BY TABLE_NAME, PRIVILEGE_TYPE) FROM information_schema.TABLE_PRIVILEGES WHERE GRANTEE = CONCAT('''lingedge_app''@''%''') AND TABLE_SCHEMA = ? AND TABLE_NAME IN ('certificates','signing_keys','system_settings','users') AND PRIVILEGE_TYPE IN ('SELECT','UPDATE')", e.dbName)
	for _, tb := range []string{"certificates", "signing_keys", "system_settings", "users"} {
		for _, p := range []string{"SELECT", "UPDATE"} {
			if !strings.Contains(grants, tb+":"+p) {
				t.Errorf("lingedge_app 缺少 %s 的 %s 授权: %s", tb, p, grants)
			}
		}
	}
	if n := e.rootStr(t, "SELECT COUNT(*) FROM information_schema.TABLE_PRIVILEGES WHERE GRANTEE = CONCAT('''lingedge_maint''@''%''') AND TABLE_SCHEMA = ? AND TABLE_NAME IN ('certificates','signing_keys','system_settings','users')", e.dbName); n != "0" {
		t.Errorf("lingedge_maint 不应对目标表有授权: %s", n)
	}
}

func openAll(t *testing.T, box *secretbox.Box, e *env, want map[[3]string]string) {
	t.Helper()
	for k, pt := range want {
		enc := e.readEnc(t, k[0], k[1], k[2])
		got, err := box.Open(ctxT(t), enc, secretbox.RecordContext{Table: k[0], Column: k[1], UID: k[2]})
		if err != nil {
			t.Fatalf("解密 %v 失败: %v", k, err)
		}
		if string(got) != pt {
			t.Fatalf("解密 %v 明文不符", k)
		}
	}
}

// 端到端轮换：写入 → 新增 k2 → 移除 k1 被拒 → RewrapAll（小批量跨表游标）→ 移除放行 → 仅含 k2 的主密钥可解全部。
func TestRotationEndToEnd(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	k1, k2 := newKey(t), newKey(t)
	box1 := newBox(t, map[string]string{"k1": k1}, "k1")
	want := e.seed(t, box1, 13) // 4 表 × 13 = 52 条
	store := e.store(t)

	box2 := newBox(t, map[string]string{"k1": k1, "k2": k2}, "k2")
	if n, err := store.CountByKEK(ctx, "k1"); err != nil || n != 52 {
		t.Fatalf("CountByKEK(k1) = %d, %v，期望 52", n, err)
	}
	if n, _ := store.CountByKEK(ctx, "k2"); n != 0 {
		t.Fatalf("CountByKEK(k2) = %d，期望 0", n)
	}
	// 记下 nonce/ct 段：重包装只能改 kek_id 与 wrapped_dek。
	segs := map[[3]string][]string{}
	for k := range want {
		segs[k] = strings.Split(e.readEnc(t, k[0], k[1], k[2]), ":")
	}

	// 接线真实引用计数：仍有引用时拒绝移除。
	rc := secretbox.RemovalCheck{KEKID: "k1", CurrentKEKID: "k2"}
	if err := secretbox.CheckKEKRemovable(ctx, store, rc); !errors.Is(err, secretbox.ErrKEKInUse) || !strings.Contains(err.Error(), "52") {
		t.Fatalf("应因仍有 52 条引用而拒绝: %v", err)
	}
	if err := secretbox.CheckKEKRemovable(ctx, store, secretbox.RemovalCheck{KEKID: "k2", CurrentKEKID: "k2"}); !errors.Is(err, secretbox.ErrKEKIsCurrent) {
		t.Fatalf("当前 KEK 不得移除: %v", err)
	}

	rep, err := box2.RewrapAll(ctx, store, "k1", "k2", 5)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rewrapped != 52 || rep.Scanned != 52 || rep.Skipped != 0 || len(rep.Failures) != 0 {
		t.Fatalf("报告: %+v", rep)
	}
	for k := range want {
		now := strings.Split(e.readEnc(t, k[0], k[1], k[2]), ":")
		old := segs[k]
		if now[1] != "k2" || now[3] != old[3] || now[4] != old[4] || now[2] == old[2] {
			t.Fatalf("%v: 只应改 kek_id 与 wrapped_dek", k)
		}
		if k[0] == "certificates" || k[0] == "signing_keys" {
			if id := e.rootStr(t, "SELECT enc_key_id FROM §.`"+k[0]+"` WHERE uid = ?", k[2]); id != "k2" {
				t.Fatalf("%v: enc_key_id = %q，期望 k2", k, id)
			}
		}
	}
	// 可重入：再跑一轮没有任何工作。
	if rep2, err := box2.RewrapAll(ctx, store, "k1", "k2", 5); err != nil || rep2.Scanned != 0 {
		t.Fatalf("重跑应为空: %+v %v", rep2, err)
	}
	if n, _ := store.CountByKEK(ctx, "k1"); n != 0 {
		t.Fatalf("CountByKEK(k1) = %d，期望 0", n)
	}
	if n, _ := store.CountByKEK(ctx, "k2"); n != 52 {
		t.Fatalf("CountByKEK(k2) = %d，期望 52", n)
	}

	// 放行：零引用；但保留备份仍需要 k1 时仍拒绝，显式放弃备份兼容才放行。
	if err := secretbox.CheckKEKRemovable(ctx, store, rc); err != nil {
		t.Fatalf("零引用应放行: %v", err)
	}
	rc.BackupRequired = []string{"k1"}
	if err := secretbox.CheckKEKRemovable(ctx, store, rc); !errors.Is(err, secretbox.ErrKEKRequiredByBackup) {
		t.Fatalf("备份仍需要应拒绝: %v", err)
	}
	rc.DropBackupCompat = true
	if err := secretbox.CheckKEKRemovable(ctx, store, rc); err != nil {
		t.Fatalf("显式放弃备份兼容应放行: %v", err)
	}

	// 仅含 k2 的主密钥文件可解密全部数据（k1 已可从主密钥文件移除）。
	onlyK2 := newBox(t, map[string]string{"k2": k2}, "k2")
	openAll(t, onlyK2, e, want)
	// 反向：仅含 k1 的无法再解（证明确实已重包装而不是原样保留）。
	for k := range want {
		if _, err := box1.Open(ctx, e.readEnc(t, k[0], k[1], k[2]), secretbox.RecordContext{Table: k[0], Column: k[1], UID: k[2]}); !errors.Is(err, secretbox.ErrKEKNotFound) {
			t.Fatalf("仅含 k1 应报 ErrKEKNotFound: %v", err)
		}
		break
	}
}

// hookStore 包装真实 Store，在 ListByKEK 返回后/Replace 之前注入钩子（模拟真实并发写入者与中断）。
type hookStore struct {
	secretbox.RecordStore
	afterList func([]secretbox.Record)
	beforeRep func(secretbox.Record)
}

func (h *hookStore) ListByKEK(ctx context.Context, kek string, after secretbox.Record, limit int) ([]secretbox.Record, error) {
	recs, err := h.RecordStore.ListByKEK(ctx, kek, after, limit)
	if err == nil && h.afterList != nil {
		h.afterList(recs)
	}
	return recs, err
}

func (h *hookStore) Replace(ctx context.Context, old secretbox.Record, newEnc, newKEK string) (bool, error) {
	if h.beforeRep != nil {
		h.beforeRep(old)
	}
	return h.RecordStore.Replace(ctx, old, newEnc, newKEK)
}

// CAS 并发冲突：扫描之后、写回之前，另一个写入者（root 连接，真实并发）改写了记录——重包装不得覆盖它。
func TestCASConflictDoesNotClobber(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	k1, k2 := newKey(t), newKey(t)
	box1 := newBox(t, map[string]string{"k1": k1}, "k1")
	box2 := newBox(t, map[string]string{"k1": k1, "k2": k2}, "k2")
	want := e.seed(t, box1, 3)
	store := e.store(t)

	// 选一条 certificates 记录作为被并发改写者：写入者用同样的 k1 写入了「新」密文（新明文）。
	var victim [3]string
	for k := range want {
		if k[0] == "certificates" {
			victim = k
			break
		}
	}
	rc := secretbox.RecordContext{Table: victim[0], Column: victim[1], UID: victim[2]}
	fresh, err := box1.Seal(ctx, []byte("rewritten-by-concurrent-writer"), rc)
	if err != nil {
		t.Fatal(err)
	}
	var fired atomic.Bool
	hs := &hookStore{RecordStore: store, beforeRep: func(old secretbox.Record) {
		if old.UID == victim[2] && fired.CompareAndSwap(false, true) {
			e.root(t, "UPDATE §.certificates SET private_key_enc = ? WHERE uid = ?", fresh, victim[2])
		}
	}}
	rep, err := box2.RewrapAll(ctx, hs, "k1", "k2", 4)
	if err != nil {
		t.Fatal(err)
	}
	if !fired.Load() || rep.Skipped != 1 || rep.Rewrapped != 11 {
		t.Fatalf("应恰有 1 条因 CAS 被跳过、其余 11 条重包装: %+v fired=%v", rep, fired.Load())
	}
	if got := e.readEnc(t, victim[0], victim[1], victim[2]); got != fresh {
		t.Fatal("被并发改写的记录被重包装覆盖了")
	}
	// 下一轮会再扫到它（仍是 k1）并完成。
	rep, err = box2.RewrapAll(ctx, store, "k1", "k2", 4)
	if err != nil || rep.Rewrapped != 1 || rep.Skipped != 0 {
		t.Fatalf("第二轮应补上被跳过的那条: %+v %v", rep, err)
	}
	pt, err := newBox(t, map[string]string{"k2": k2}, "k2").Open(ctx, e.readEnc(t, victim[0], victim[1], victim[2]), rc)
	if err != nil || string(pt) != "rewritten-by-concurrent-writer" {
		t.Fatalf("并发写入者的新明文应保留: %q %v", pt, err)
	}
}

// CAS 的相等比较必须区分大小写（utf8mb4_0900_ai_ci 默认不区分）：大小写变体的「旧密文」不得命中。
func TestCASIsCaseSensitive(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	k1, k2 := newKey(t), newKey(t)
	box1 := newBox(t, map[string]string{"k1": k1}, "k1")
	box2 := newBox(t, map[string]string{"k1": k1, "k2": k2}, "k2")
	want := e.seed(t, box1, 1)
	store := e.store(t)
	for k := range want {
		if k[0] != "users" {
			continue
		}
		enc := e.readEnc(t, k[0], k[1], k[2])
		rc := secretbox.RecordContext{Table: k[0], Column: k[1], UID: k[2]}
		newEnc, changed, err := box2.Rewrap(ctx, enc, rc, "k2")
		if err != nil || !changed {
			t.Fatal(err)
		}
		variant := strings.ToUpper(enc[3:]) // 保持 v1: 前缀，其余大写：与原文在 ai_ci 下相等、逐字节不等
		variant = enc[:3] + variant
		if variant == enc {
			t.Skip("密文无可变换的小写字符")
		}
		ok, err := store.Replace(ctx, secretbox.Record{Table: k[0], Column: k[1], UID: k[2], Enc: variant}, newEnc, "k2")
		if err != nil || ok {
			t.Fatalf("大小写变体不得命中 CAS: ok=%v err=%v", ok, err)
		}
		if ok, err := store.Replace(ctx, secretbox.Record{Table: k[0], Column: k[1], UID: k[2], Enc: enc}, newEnc, "k2"); err != nil || !ok {
			t.Fatalf("逐字节相等应命中: ok=%v err=%v", ok, err)
		}
	}
}

// AAD 搬迁：把 A 的密文搬到 B（同表同列不同 uid）——重包装必须失败并记入报告，B 仍以 k1 计数，k1 因此不可移除；A 正常。
func TestRewrapRejectsRelocatedCiphertext(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	k1, k2 := newKey(t), newKey(t)
	box1 := newBox(t, map[string]string{"k1": k1}, "k1")
	box2 := newBox(t, map[string]string{"k1": k1, "k2": k2}, "k2")
	want := e.seed(t, box1, 2)
	store := e.store(t)

	var certs []string
	for k := range want {
		if k[0] == "certificates" {
			certs = append(certs, k[2])
		}
	}
	sort.Strings(certs)
	a, b := certs[0], certs[1]
	e.root(t, "UPDATE §.certificates SET private_key_enc = (SELECT c.private_key_enc FROM (SELECT private_key_enc FROM §.certificates WHERE uid = ?) c) WHERE uid = ?", a, b)

	rep, err := box2.RewrapAll(ctx, store, "k1", "k2", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Failures) != 1 || !strings.Contains(rep.Failures[0], "certificates|private_key_enc|"+b) || rep.Rewrapped != 7 {
		t.Fatalf("搬迁的记录应失败且其余成功: %+v", rep)
	}
	if id, _ := secretbox.KeyID(e.readEnc(t, "certificates", "private_key_enc", b)); id != "k1" {
		t.Fatalf("搬迁记录不得被重包装洗白: %s", id)
	}
	if err := secretbox.CheckKEKRemovable(ctx, store, secretbox.RemovalCheck{KEKID: "k1", CurrentKEKID: "k2"}); !errors.Is(err, secretbox.ErrKEKInUse) || !strings.Contains(err.Error(), "1") {
		t.Fatalf("残留 1 条 k1 引用应拒绝移除: %v", err)
	}
}

// 中断后可重入：处理到一半被取消（模拟进程被杀/任务被中断），再次运行补齐；总写回数 = 总记录数，没有重复或遗漏。
func TestRewrapResumeAfterInterruption(t *testing.T) {
	e := newEnv(t)
	k1, k2 := newKey(t), newKey(t)
	box1 := newBox(t, map[string]string{"k1": k1}, "k1")
	box2 := newBox(t, map[string]string{"k1": k1, "k2": k2}, "k2")
	want := e.seed(t, box1, 6) // 24 条
	store := e.store(t)

	ctx, cancel := context.WithCancel(ctxT(t))
	defer cancel()
	var replaced atomic.Int32
	hs := &hookStore{RecordStore: store, beforeRep: func(secretbox.Record) {
		if replaced.Add(1) == 10 {
			cancel() // 第 10 次写回之前中断
		}
	}}
	rep1, err := box2.RewrapAll(ctx, hs, "k1", "k2", 4)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应因取消而中断: %v", err)
	}
	if rep1.Rewrapped == 0 || rep1.Rewrapped >= 24 {
		t.Fatalf("应只完成一部分: %+v", rep1)
	}
	mid, _ := store.CountByKEK(ctxT(t), "k1")
	if int(mid)+rep1.Rewrapped != 24 {
		t.Fatalf("中断后库内状态不一致: 剩余 %d + 已写 %d != 24", mid, rep1.Rewrapped)
	}
	if err := secretbox.CheckKEKRemovable(ctxT(t), store, secretbox.RemovalCheck{KEKID: "k1", CurrentKEKID: "k2"}); !errors.Is(err, secretbox.ErrKEKInUse) {
		t.Fatalf("中断后仍有引用，不得放行: %v", err)
	}
	rep2, err := box2.RewrapAll(ctxT(t), store, "k1", "k2", 4)
	if err != nil || rep2.Rewrapped != int(mid) || len(rep2.Failures) != 0 {
		t.Fatalf("续跑应补齐剩余 %d 条: %+v %v", mid, rep2, err)
	}
	if n, _ := store.CountByKEK(ctxT(t), "k1"); n != 0 {
		t.Fatalf("续跑后仍有 %d 条 k1 引用", n)
	}
	openAll(t, newBox(t, map[string]string{"k2": k2}, "k2"), e, want)
}

// 软删除/NULL 列：users 无 TOTP（NULL）不被扫描；已软删除的行仍计入引用（行与备份里都还在）。
func TestNullColumnsIgnoredAndSoftDeletedCounted(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	k1 := newKey(t)
	box1 := newBox(t, map[string]string{"k1": k1}, "k1")
	want := e.seed(t, box1, 1)
	e.root(t, "INSERT INTO §.users (uid, user_type, email, display_name, password_hash) VALUES (?, 'tenant', 'nulltotp@example.test', 'n', 'x')", e.ids.MustNew())
	for k := range want {
		if k[0] == "users" {
			e.root(t, "UPDATE §.users SET deleted_at = NOW(3) WHERE uid = ?", k[2])
		}
	}
	store := e.store(t)
	if n, err := store.CountByKEK(ctx, "k1"); err != nil || n != 4 {
		t.Fatalf("CountByKEK = %d, %v，期望 4（含软删除行）", n, err)
	}
}

// kek_id 里的 '_' 与 '-' 不得被当作通配符：k_1 与 kx1 是两个不同的 KEK。
func TestKEKIDIsNotAPattern(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	kA, kB := newKey(t), newKey(t)
	boxA := newBox(t, map[string]string{"k_1": kA}, "k_1")
	boxB := newBox(t, map[string]string{"kx1": kB}, "kx1")
	e.seed(t, boxA, 1)
	e.seed(t, boxB, 2)
	store := e.store(t)
	for kek, want := range map[string]int64{"k_1": 4, "kx1": 8, "k-1": 0, "k%": 0} {
		n, err := store.CountByKEK(ctx, kek)
		if kek == "k%" {
			if err == nil {
				t.Errorf("非法 kek_id 应被拒绝")
			}
			continue
		}
		if err != nil || n != want {
			t.Errorf("CountByKEK(%q) = %d, %v，期望 %d", kek, n, err, want)
		}
	}
}
