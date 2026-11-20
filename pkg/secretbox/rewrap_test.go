package secretbox_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// memStore 是测试用的 RecordStore：按（表,列,uid）有序保存记录与 enc_key_id，语义与 SQL 仓储一致（Replace 为 CAS）。
// 它只替身「库」，加解密一律走真实的 Box/FileKeyProvider。
type memStore struct {
	mu   sync.Mutex
	rec  map[[3]string]string // key → enc
	hook func()               // ListByKEK 返回前的钩子（模拟并发改动）
}

func recLess(a, b secretbox.Record) bool {
	if a.Table != b.Table {
		return a.Table < b.Table
	}
	if a.Column != b.Column {
		return a.Column < b.Column
	}
	return a.UID < b.UID
}

func newMemStore() *memStore { return &memStore{rec: map[[3]string]string{}} }

func (m *memStore) put(r secretbox.Record) { m.rec[[3]string{r.Table, r.Column, r.UID}] = r.Enc }

func (m *memStore) ListByKEK(_ context.Context, kek string, after secretbox.Record, limit int) ([]secretbox.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []secretbox.Record
	for k, enc := range m.rec {
		id, _ := secretbox.KeyID(enc)
		if id != kek {
			continue
		}
		r := secretbox.Record{Table: k[0], Column: k[1], UID: k[2], Enc: enc}
		if after != (secretbox.Record{}) && !recLess(after, r) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return recLess(out[i], out[j]) })
	if len(out) > limit {
		out = out[:limit]
	}
	if m.hook != nil {
		m.hook()
	}
	return out, nil
}

func (m *memStore) Replace(_ context.Context, old secretbox.Record, newEnc, newKEK string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := [3]string{old.Table, old.Column, old.UID}
	if m.rec[k] != old.Enc {
		return false, nil
	}
	if id, _ := secretbox.KeyID(newEnc); id != newKEK {
		return false, errors.New("enc_key_id 与密文内 kek_id 不一致")
	}
	m.rec[k] = newEnc
	return true, nil
}

func (m *memStore) CountByKEK(_ context.Context, kek string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, enc := range m.rec {
		if id, _ := secretbox.KeyID(enc); id == kek {
			n++
		}
	}
	return n, nil
}

func keyFile(ids map[string]string, current string) string {
	var order []string
	for id := range ids {
		order = append(order, id)
	}
	sort.Strings(order)
	var b strings.Builder
	b.WriteString("@current " + current + "\n")
	for _, id := range order {
		b.WriteString(id + ":" + ids[id] + "\n")
	}
	return b.String()
}

// AC3：新增 KEK → rewrap → 移除旧 KEK，数据仍可解密；DEK 之外的密文段原样保留。
func TestRotationEndToEnd(t *testing.T) {
	ctx := context.Background()
	k1, k2 := newKey(t), newKey(t)
	store := newMemStore()

	// 1) 只有 k1：写入若干记录（不同表/列）。
	p1 := newProvider(t, keyFile(map[string]string{"k1": k1}, "k1"))
	box1 := newBox(t, p1)
	want := map[[3]string]string{}
	for i := 0; i < 250; i++ {
		tbl, col := "certificates", "private_key_enc"
		if i%3 == 0 {
			tbl, col = "provider_credentials", "credential_enc"
		}
		uid := fmt.Sprintf("01J%023d", i)
		pt := fmt.Sprintf("secret-%d", i)
		enc, err := box1.Seal(ctx, []byte(pt), secretbox.RecordContext{Table: tbl, Column: col, UID: uid})
		if err != nil {
			t.Fatal(err)
		}
		store.put(secretbox.Record{Table: tbl, Column: col, UID: uid, Enc: enc})
		want[[3]string{tbl, col, uid}] = pt
	}

	// 2) 新增 k2（当前）：新写入用 k2，旧数据仍可由含 k1 的主密钥文件解密。
	p2 := newProvider(t, keyFile(map[string]string{"k1": k1, "k2": k2}, "k2"))
	box2 := newBox(t, p2)
	newRC := secretbox.RecordContext{Table: "certificates", Column: "private_key_enc", UID: "01JNEW00000000000000000000"}
	encNew, _ := box2.Seal(ctx, []byte("fresh"), newRC)
	store.put(secretbox.Record{Table: newRC.Table, Column: newRC.Column, UID: newRC.UID, Enc: encNew})
	if id, _ := secretbox.KeyID(encNew); id != "k2" {
		t.Fatalf("新写入应使用 k2: %s", id)
	}
	snapshot := map[[3]string]string{}
	for k, v := range store.rec {
		snapshot[k] = v
	}

	// 3) 旧 KEK 未全部重包装前不可移除（库内引用 + 当前 KEK 两道拒绝）。
	err := secretbox.CheckKEKRemovable(ctx, store, secretbox.RemovalCheck{KEKID: "k1", CurrentKEKID: "k2"})
	if !errors.Is(err, secretbox.ErrKEKInUse) || !strings.Contains(err.Error(), "250") {
		t.Fatalf("重包装前不得移除 k1: %v", err)
	}
	if err := secretbox.CheckKEKRemovable(ctx, store, secretbox.RemovalCheck{KEKID: "k2", CurrentKEKID: "k2"}); !errors.Is(err, secretbox.ErrKEKIsCurrent) {
		t.Fatalf("当前 KEK 不可移除: %v", err)
	}
	// 只有 k2 的主密钥此时无法解密旧数据（证明「移除」的危害，也是上面拦截的理由）。
	onlyK2 := newBox(t, newProvider(t, keyFile(map[string]string{"k2": k2}, "k2")))
	for k, enc := range snapshot {
		if id, _ := secretbox.KeyID(enc); id == "k1" {
			if _, err := onlyK2.Open(ctx, enc, secretbox.RecordContext{Table: k[0], Column: k[1], UID: k[2]}); !errors.Is(err, secretbox.ErrKEKNotFound) {
				t.Fatalf("k1 数据在缺 k1 时应无法解密: %v", err)
			}
			break
		}
	}

	// 4) secrets.rewrap：分批（7 条一批）重包装。
	rep, err := box2.RewrapAll(ctx, store, "k1", "k2", 7)
	if err != nil || rep.Scanned != 250 || rep.Rewrapped != 250 || len(rep.Failures) != 0 || rep.Skipped != 0 {
		t.Fatalf("RewrapAll: %+v %v", rep, err)
	}
	// 幂等：再跑一遍无事可做。
	if rep, err = box2.RewrapAll(ctx, store, "k1", "k2", 100); err != nil || rep.Scanned != 0 {
		t.Fatalf("重复运行应为空: %+v %v", rep, err)
	}
	// 只重包装 DEK：nonce、ct+tag 段与旧值逐字节相同；wrapped_dek 段与 kek_id 变化。
	for k, old := range snapshot {
		cur := store.rec[k]
		po, pc := strings.Split(old, ":"), strings.Split(cur, ":")
		if pc[1] != "k2" || pc[3] != po[3] || pc[4] != po[4] {
			t.Fatalf("%v: 重包装应只改 kek_id/wrapped_dek", k)
		}
		if po[1] == "k1" && pc[2] == po[2] {
			t.Fatalf("%v: wrapped_dek 未变化", k)
		}
	}

	// 5) 现在可移除 k1；仅含 k2 的主密钥仍能解密全部数据。
	if err := secretbox.CheckKEKRemovable(ctx, store, secretbox.RemovalCheck{KEKID: "k1", CurrentKEKID: "k2"}); err != nil {
		t.Fatalf("重包装后应允许移除: %v", err)
	}
	for k, pt := range want {
		got, err := onlyK2.Open(ctx, store.rec[k], secretbox.RecordContext{Table: k[0], Column: k[1], UID: k[2]})
		if err != nil || string(got) != pt {
			t.Fatalf("移除 k1 后 %v 解密失败: %v", k, err)
		}
	}
}

// MIN-05：被保留备份需要的 KEK 拒绝移除，除非显式放弃备份兼容。
func TestRemovalBlockedByBackup(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	c := secretbox.RemovalCheck{KEKID: "k1", CurrentKEKID: "k2", BackupRequired: []string{"k0", "k1"}}
	if err := secretbox.CheckKEKRemovable(ctx, store, c); !errors.Is(err, secretbox.ErrKEKRequiredByBackup) {
		t.Fatalf("备份仍需要应拒绝: %v", err)
	}
	c.DropBackupCompat = true
	if err := secretbox.CheckKEKRemovable(ctx, store, c); err != nil {
		t.Fatalf("显式放弃备份兼容应放行: %v", err)
	}
	// 即使放弃备份兼容，库内仍有引用也不可移除。
	k := newKey(t)
	box := newBox(t, newProvider(t, "k1:"+k+"\n"))
	enc, _ := box.Seal(ctx, []byte("x"), rc)
	store.put(secretbox.Record{Table: rc.Table, Column: rc.Column, UID: rc.UID, Enc: enc})
	if err := secretbox.CheckKEKRemovable(ctx, store, c); !errors.Is(err, secretbox.ErrKEKInUse) {
		t.Fatalf("库内仍引用应拒绝: %v", err)
	}
}

// 单条记录失败不终止、不死循环；并发改动（CAS 失败）计入 Skipped；失败信息不含密文/密钥。
func TestRewrapAllFailuresAndRace(t *testing.T) {
	ctx := context.Background()
	k1, k2 := newKey(t), newKey(t)
	box := newBox(t, newProvider(t, keyFile(map[string]string{"k1": k1, "k2": k2}, "k1")))
	store := newMemStore()
	for i := 0; i < 5; i++ {
		r := secretbox.RecordContext{Table: "t", Column: "c", UID: fmt.Sprintf("u%d", i)}
		enc, _ := box.Seal(ctx, []byte("p"), r)
		store.put(secretbox.Record{Table: "t", Column: "c", UID: r.UID, Enc: enc})
	}
	// u1：把密文搬到 u1（AAD 不符）→ 重包装失败
	store.rec[[3]string{"t", "c", "u1"}] = store.rec[[3]string{"t", "c", "u0"}]
	// u3：并发被别人改写（钩子在列出之后、写回之前改动）
	store.hook = func() {
		store.hook = nil
		r := secretbox.RecordContext{Table: "t", Column: "c", UID: "u3"}
		enc, _ := box.Seal(ctx, []byte("changed"), r)
		store.rec[[3]string{"t", "c", "u3"}] = enc
	}
	rep, err := box.RewrapAll(ctx, store, "k1", "k2", 100)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rewrapped != 3 || len(rep.Failures) != 1 || rep.Skipped != 1 || !strings.Contains(rep.Failures[0], "t|c|u1") {
		t.Fatalf("报告不符: %+v", rep)
	}
	if strings.Contains(rep.Failures[0], k1) || strings.Contains(rep.Failures[0], "v1:") {
		t.Fatal("失败信息泄露密文/密钥")
	}
	// 失败记录仍以 k1 包装 → 此时 k1 不可移除。
	if err := secretbox.CheckKEKRemovable(ctx, store, secretbox.RemovalCheck{KEKID: "k1", CurrentKEKID: "k2"}); !errors.Is(err, secretbox.ErrKEKInUse) {
		t.Fatalf("存在未重包装记录时必须拒绝移除: %v", err)
	}
	// 参数校验
	if _, err := box.RewrapAll(ctx, store, "k1", "k1", 1); err == nil {
		t.Fatal("源=目标应报错")
	}
	// 取消 ctx
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := box.RewrapAll(cctx, store, "k1", "k2", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("应响应取消: %v", err)
	}
}

// Rewrap 单值：AAD 不符、目标 KEK 不存在、无效目标都失败，且不改变原值。
func TestRewrapSingleNegative(t *testing.T) {
	ctx := context.Background()
	box := newBox(t, newProvider(t, keyFile(map[string]string{"k1": newKey(t), "k2": newKey(t)}, "k1")))
	enc, _ := box.Seal(ctx, []byte("p"), rc)
	other := secretbox.RecordContext{Table: rc.Table, Column: rc.Column, UID: "other"}
	if _, _, err := box.Rewrap(ctx, enc, other, "k2"); !errors.Is(err, secretbox.ErrDecrypt) {
		t.Errorf("AAD 不符: %v", err)
	}
	if _, _, err := box.Rewrap(ctx, enc, rc, "k9"); !errors.Is(err, secretbox.ErrKEKNotFound) {
		t.Errorf("目标 KEK 不存在: %v", err)
	}
	if _, _, err := box.Rewrap(ctx, enc, rc, "K!"); err == nil {
		t.Error("非法目标")
	}
	if got, err := box.Open(ctx, enc, rc); err != nil || string(got) != "p" {
		t.Errorf("原值应不受影响: %v", err)
	}
}
