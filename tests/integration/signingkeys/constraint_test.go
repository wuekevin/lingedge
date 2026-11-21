package signingkeysit

import (
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/tests/apistack"
)

// 数据库层的约束（迁移 000005，无需新迁移）：每个 purpose 至多一个 active（生成列 active_key 上的唯一索引）；
// pending/retiring/revoked 可有多行；purpose/algorithm/status 受 CHECK 约束；用真实 lingedge_app 账号经应用连接池验证（不只是 root）。
func TestSigningKeysStatusAndActiveUniqueness(t *testing.T) {
	w := apistack.New(t)
	ctx := ctxT(t)
	n := 0
	insert := func(purpose, alg, status string) error {
		n++
		uid, _ := apistack.IDs.New()
		_, err := w.App.Executor(ctx).Exec(ctx, db.Q("INSERT INTO signing_keys (uid, purpose, key_id, algorithm, public_key, private_key_enc, enc_key_id, status) VALUES (?, ?, ?, ?, ?, ?, 'k1', ?)"),
			uid, purpose, uid[:16]+string(rune('a'+n)), alg, []byte{1, 2, 3}, "v1:k1:x:y:z", status)
		return err
	}
	if err := insert("config_signing", "ed25519", "active"); err != nil {
		t.Fatal(err)
	}
	// 同 purpose 的第二个 active → 重复键（由 uk_signing_keys_active 保证）。
	if err := insert("config_signing", "ed25519", "active"); !db.IsDuplicate(err) {
		t.Fatalf("同 purpose 第二个 active 应被唯一索引拒绝: %v", err)
	}
	// 其他状态可有多行（新 key 先 pending、旧 key 退役 retiring、已吊销 revoked）。
	for _, st := range []string{"pending", "pending", "retiring", "retiring", "revoked", "revoked"} {
		if err := insert("config_signing", "ed25519", st); err != nil {
			t.Fatalf("%s 应允许多行: %v", st, err)
		}
	}
	// 不同 purpose 各自一个 active。
	if err := insert("mtls_ca", "ecdsa_p256", "active"); err != nil {
		t.Fatal(err)
	}
	if err := insert("grpc_server", "ecdsa_p256", "active"); err != nil {
		t.Fatal(err)
	}
	if err := insert("mtls_ca", "ecdsa_p256", "active"); !db.IsDuplicate(err) {
		t.Fatalf("mtls_ca 第二个 active 应被拒绝: %v", err)
	}
	// 状态流转也受约束：把一个 pending 置为 active 而已有 active 时被拒；先让旧的 active 退役则成功（轮换的原子顺序）。
	uidPending := w.RootStr("SELECT uid FROM §.signing_keys WHERE purpose = 'config_signing' AND status = 'pending' ORDER BY id LIMIT 1")
	if _, err := w.App.Executor(ctx).Exec(ctx, db.Q("UPDATE signing_keys SET status = 'active' WHERE uid = ?"), uidPending); !db.IsDuplicate(err) {
		t.Fatalf("已有 active 时把 pending 置 active 应被拒: %v", err)
	}
	w.Root("UPDATE §.signing_keys SET status = 'retiring' WHERE purpose = 'config_signing' AND status = 'active'")
	if _, err := w.App.Executor(ctx).Exec(ctx, db.Q("UPDATE signing_keys SET status = 'active' WHERE uid = ?"), uidPending); err != nil {
		t.Fatalf("旧 active 退役后应可激活新 key: %v", err)
	}
	if c := w.RootInt("SELECT COUNT(*) FROM §.signing_keys WHERE purpose = 'config_signing' AND status = 'active'"); c != 1 {
		t.Errorf("config_signing 应恰一个 active: %d", c)
	}
	// CHECK：非法 purpose / 算法 / 状态被拒。
	for _, c := range []struct{ p, a, s string }{{"release_signing", "ed25519", "pending"}, {"config_signing", "rsa", "pending"}, {"config_signing", "ed25519", "enabled"}} {
		if err := insert(c.p, c.a, c.s); err == nil {
			t.Errorf("%+v 应被 CHECK 约束拒绝", c)
		}
	}
	// 「发行签名私钥离线不入库」在 schema 上的体现：purpose 取值里没有发行清单用途（信任域①）。
	if err := insert("release_manifest", "ed25519", "active"); err == nil {
		t.Error("signing_keys 不得接受发行清单（信任域①）用途")
	}
}
