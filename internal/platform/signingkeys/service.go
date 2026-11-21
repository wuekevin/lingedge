package signingkeys

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/audit"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/signingkeys/repository"
	"github.com/wuekevin/lingedge/internal/platform/sysconfig"
	"github.com/wuekevin/lingedge/pkg/secretbox"
	"github.com/wuekevin/lingedge/pkg/signing"
)

// Purpose 是 signing_keys.purpose 的取值（迁移 000005 的 CHECK）。
type Purpose string

// 密钥用途：config_signing = 信任域②（配置修订签名，Ed25519）；mtls_ca = 信任域③（节点 mTLS 私有 CA，ECDSA P-256）；
// grpc_server = Server gRPC 叶证书（由 mtls_ca 签发）。信任域①（发行清单签名）的私钥离线，不在 Server、不在本表（ADR-012 §2）。
const (
	PurposeConfigSigning Purpose = "config_signing"
	PurposeMTLSCA        Purpose = "mtls_ca"
	PurposeGRPCServer    Purpose = "grpc_server"
)

// 状态（迁移 000005 的 CHECK）。
const (
	StatusPending  = "pending"
	StatusActive   = "active"
	StatusRetiring = "retiring"
	StatusRevoked  = "revoked"
)

// 算法（迁移 000005 的 CHECK）。
const (
	AlgEd25519 = "ed25519"
	AlgP256    = "ecdsa_p256"
)

const (
	tableName      = "signing_keys"
	columnName     = "private_key_enc"
	lineageKey     = "deploy.lineage"
	lineageGroup   = "deploy"
	initialLineage = "1"
	maxInitRetries = 8
)

// 错误。
var (
	ErrNotInitialized    = errors.New("signingkeys: 签名密钥尚未初始化（需先完成安装向导的「内部身份与密钥初始化」）")
	ErrInvalidCAValidity = fmt.Errorf("signingkeys: CA 有效期必须在 %d–%d 年之间", MinCAYears, MaxCAYears)
	ErrInvalidServerURL  = errors.New("signingkeys: server_url 非法（须为带主机的 URL，主机为域名或 IP）")
	ErrWrongRole         = repository.ErrWrongRole
	// ErrMasterKeyMissing：主密钥不可用（未配置、文件缺失、已关闭或无法完成一次加解密自检）——拒绝初始化，不写入任何东西。
	ErrMasterKeyMissing = secretbox.ErrMasterKeyMissing
)

// SecretProbe 是测试探针：每当有私钥明文缓冲被物化到内存时调用（name 标识用途）；测试据此保留切片引用，在操作结束后断言其已被清零。
// 生产装配不设置。探针拿到的是活切片：实现不得修改它。
type SecretProbe func(name string, buf []byte)

// Options 是 New 的参数。
type Options struct {
	// Pool 必须是 app 池（lingedge_app 对 signing_keys/system_settings 有 CRUD、对 audit_logs 有 INSERT，无需更高权限账号）。
	Pool  db.Pool
	Box   *secretbox.Box
	Audit audit.Recorder
	IDs   interface{ New() (string, error) }
	Clock clock.Clock
	Log   *zap.Logger
	Probe SecretProbe
	// ManifestOnly：只用于导出路由清单（处理函数不会被调用），允许其余依赖为空。
	ManifestOnly bool
}

// Service 是 signing_keys 服务：初始化、签名、公开元数据读取、CA 有效期检查。
type Service struct {
	pool  db.Pool
	repo  *repository.Repo
	box   *secretbox.Box
	audit audit.Recorder
	ids   interface{ New() (string, error) }
	clk   clock.Clock
	log   *zap.Logger
	probe SecretProbe
}

// New 创建 Service。Box 为空（没有主密钥）时拒绝：没有 KEK 就不可能加密私钥。
func New(o Options) (*Service, error) {
	if o.ManifestOnly {
		return &Service{}, nil
	}
	if o.Box == nil {
		return nil, ErrMasterKeyMissing
	}
	if o.Pool == nil || o.Audit == nil || o.IDs == nil {
		return nil, errors.New("signingkeys: 缺少必需依赖（Pool、Audit、IDs）")
	}
	repo, err := repository.New(o.Pool)
	if err != nil {
		return nil, err
	}
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	if o.Log == nil {
		o.Log = zap.NewNop()
	}
	return &Service{pool: o.Pool, repo: repo, box: o.Box, audit: o.Audit, ids: o.IDs, clk: o.Clock, log: o.Log.Named("signingkeys"), probe: o.Probe}, nil
}

// ---- 内存中的私钥明文：用后清零 ----

// scratch 收集一次操作中物化的私钥明文缓冲，操作结束时统一清零；探针（测试）在物化时拿到引用。
//
// 已知局限（诚实声明）：应用自己持有的明文字节（PKCS#8 DER、Ed25519 私钥切片）全部在用后清零；但 *ecdsa.PrivateKey 是标准库对象，
// 其内部标量副本无法由应用可靠清零（Go 1.26 起 ecdsa.PrivateKey.D 已弃用，「修改原始值可能产生无效密钥并使内部优化失效」），
// 因此 ECDSA 私钥对象只在最小作用域内存在、不被缓存、不被传出本包，其生命周期交给 GC；这一点与 Go 标准库的内存模型一致，不是本服务可以绕过的。
type scratch struct {
	s    *Service
	bufs [][]byte
}

func (s *Service) newScratch() *scratch { return &scratch{s: s} }

func (c *scratch) hold(name string, b []byte) []byte {
	if c.s.probe != nil {
		c.s.probe(name, b)
	}
	c.bufs = append(c.bufs, b)
	return b
}

// wipe 清零全部缓冲；可重复调用。
func (c *scratch) wipe() {
	for _, b := range c.bufs {
		secretbox.Zero(b)
	}
	c.bufs = nil
}

// ---- 公开元数据 ----

// KeyInfo 是一把密钥的公开信息：公钥与指纹，绝不含私钥（也不含密文）。
type KeyInfo struct {
	KeyID     string
	Purpose   Purpose
	Algorithm string
	Status    string
	// PublicKey：ed25519 为 32 字节原始公钥；ecdsa_p256 为 PKIX（SPKI）DER。
	PublicKey []byte
	// Fingerprint：配置签名密钥 = SHA-256(原始公钥)；证书类（CA/叶证书）= SHA-256(证书 DER)（CA 的即 enroll Token 的 ca_fp）。小写十六进制。
	Fingerprint string
	// NotAfter：证书类密钥的证书到期时间；配置签名密钥为空（配置签名无过期，R-4）。
	NotAfter    *time.Time
	ActivatedAt *time.Time
	RetireAfter *time.Time
	RevokedAt   *time.Time
}

func toInfo(k repository.Key) (KeyInfo, error) {
	in := KeyInfo{KeyID: k.KeyID, Purpose: Purpose(k.Purpose), Algorithm: k.Algorithm, Status: k.Status, PublicKey: append([]byte(nil), k.PublicKey...),
		ActivatedAt: k.ActivatedAt, RetireAfter: k.RetireAfter, RevokedAt: k.RevokedAt}
	if k.CertPEM != nil && *k.CertPEM != "" {
		c, err := parseCertPEM(*k.CertPEM)
		if err != nil {
			return KeyInfo{}, fmt.Errorf("signingkeys: 密钥 %s 的证书无法解析: %w", k.KeyID, err)
		}
		in.Fingerprint = signing.CertFingerprint(c.Raw)
		na := c.NotAfter.UTC()
		in.NotAfter = &na
	} else {
		in.Fingerprint = signing.Fingerprint(k.PublicKey)
	}
	return in, nil
}

// List 分页返回全部密钥的公开信息（按创建顺序）与总数。
func (s *Service) List(ctx context.Context, limit, offset int) ([]KeyInfo, int64, error) {
	rows, total, err := s.repo.List(ctx, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("signingkeys: 读取: %w", err)
	}
	out := make([]KeyInfo, 0, len(rows))
	for _, r := range rows {
		in, err := toInfo(r)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, in)
	}
	return out, total, nil
}

// Active 返回某 purpose 当前 active 密钥的公开信息；没有返回 ErrNotInitialized。
func (s *Service) Active(ctx context.Context, p Purpose) (KeyInfo, error) {
	k, ok, err := s.repo.GetActive(ctx, string(p))
	if err != nil {
		return KeyInfo{}, fmt.Errorf("signingkeys: 读取: %w", err)
	}
	if !ok {
		return KeyInfo{}, ErrNotInitialized
	}
	return toInfo(k)
}

// CAStatus 返回私有 CA 的剩余有效期检查结果（剩余 < 1 年为 warn，ADR-003 §3）；供 `doctor`（Story 1.42）调用。
// 纯判断逻辑在 pkg/signing.CheckCAExpiry（Tools 离线也可复用）。
func (s *Service) CAStatus(ctx context.Context) (signing.CAExpiryStatus, error) {
	ca, err := s.Active(ctx, PurposeMTLSCA)
	if err != nil {
		return signing.CAExpiryStatus{}, err
	}
	if ca.NotAfter == nil {
		return signing.CAExpiryStatus{}, errors.New("signingkeys: CA 缺少证书")
	}
	return signing.CheckCAExpiry(*ca.NotAfter, s.clk.Now()), nil
}

// InitialKeyset 返回信任起点 keyset（version=1、previous_version=0、无签名，enroll 时经 TLS+CA 钉扎下发，node-protocol §8）：
// 包含全部 active/retiring/revoked 的配置签名公钥（pending 不下发）。轮换产生的后续版本属 Story 3.10。
func (s *Service) InitialKeyset(ctx context.Context) (signing.Keyset, error) {
	rows, _, err := s.repo.List(ctx, 1000, 0)
	if err != nil {
		return signing.Keyset{}, fmt.Errorf("signingkeys: 读取: %w", err)
	}
	ks := signing.Keyset{Version: 1}
	for _, r := range rows {
		if r.Purpose != string(PurposeConfigSigning) {
			continue
		}
		var st signing.KeyStatus
		switch r.Status {
		case StatusActive:
			st = signing.StatusActive
		case StatusRetiring:
			st = signing.StatusRetiring
		case StatusRevoked:
			st = signing.StatusRevoked
		default:
			continue
		}
		k := signing.Key{KeyID: r.KeyID, Algorithm: signing.AlgEd25519, PublicKey: append([]byte(nil), r.PublicKey...), Status: st}
		if r.ActivatedAt != nil {
			k.NotBefore = r.ActivatedAt.UTC()
		}
		ks.Keys = append(ks.Keys, k)
		if ks.IssuedAt.IsZero() || r.CreatedAt.Before(ks.IssuedAt) {
			ks.IssuedAt = r.CreatedAt.UTC()
		}
	}
	if len(ks.Keys) == 0 {
		return signing.Keyset{}, ErrNotInitialized
	}
	sort.Slice(ks.Keys, func(i, j int) bool { return ks.Keys[i].KeyID < ks.Keys[j].KeyID })
	return ks, nil
}

// ---- 签名 ----

// Sign 用当前 active 的配置签名密钥对清单字节签名：Ed25519(priv, "LINGEDGE-REVISION-V1\n" ‖ manifestBytes)，记录 key_id，不带过期时间（R-4）。
// 私钥只在本调用内解密到内存，返回前（含出错路径）清零。验证见 pkg/signing.VerifyRevision（节点侧复用同一实现）。
func (s *Service) Sign(ctx context.Context, manifestBytes []byte) (signing.RevisionSignature, error) {
	k, ok, err := s.repo.GetActive(ctx, string(PurposeConfigSigning))
	if err != nil {
		return signing.RevisionSignature{}, fmt.Errorf("signingkeys: 读取: %w", err)
	}
	if !ok {
		return signing.RevisionSignature{}, ErrNotInitialized
	}
	sc := s.newScratch()
	defer sc.wipe()
	priv, err := s.openEd25519(ctx, sc, k)
	if err != nil {
		return signing.RevisionSignature{}, err
	}
	return signing.SignRevision(k.KeyID, priv, manifestBytes)
}

// openEd25519 解密并解析配置签名私钥（PKCS#8）。purpose 与算法必须匹配：只有 config_signing 行、ed25519 算法才可作为配置签名密钥使用，
// 拿 mtls_ca/grpc_server（ECDSA）去签修订会在这里失败；AAD（表|列|uid）把密文绑定到所在行，挪行/改 uid 解密即失败。
func (s *Service) openEd25519(ctx context.Context, sc *scratch, k repository.Key) (ed25519.PrivateKey, error) {
	if k.Purpose != string(PurposeConfigSigning) || k.Algorithm != AlgEd25519 {
		return nil, fmt.Errorf("signingkeys: 密钥 %s（purpose=%s, algorithm=%s）不能用于配置签名", k.KeyID, k.Purpose, k.Algorithm)
	}
	der, err := s.box.Open(ctx, k.PrivateKeyEnc, secretbox.RecordContext{Table: tableName, Column: columnName, UID: k.UID})
	if err != nil {
		return nil, fmt.Errorf("signingkeys: 解密密钥 %s: %w", k.KeyID, err)
	}
	sc.hold(string(PurposeConfigSigning)+".pkcs8", der)
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("signingkeys: 解析密钥 %s: %w", k.KeyID, err)
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("signingkeys: 密钥 %s 不是 Ed25519", k.KeyID)
	}
	sc.hold(string(PurposeConfigSigning)+".ed25519", priv)
	return priv, nil
}

// openCA 解密并解析 CA 私钥与证书，仅供签发叶证书。
func (s *Service) openCA(ctx context.Context, sc *scratch, k repository.Key) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	if k.Purpose != string(PurposeMTLSCA) || k.Algorithm != AlgP256 || k.CertPEM == nil {
		return nil, nil, fmt.Errorf("signingkeys: 密钥 %s 不是 CA", k.KeyID)
	}
	der, err := s.box.Open(ctx, k.PrivateKeyEnc, secretbox.RecordContext{Table: tableName, Column: columnName, UID: k.UID})
	if err != nil {
		return nil, nil, fmt.Errorf("signingkeys: 解密 CA 私钥: %w", err)
	}
	sc.hold(string(PurposeMTLSCA)+".pkcs8", der)
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, nil, fmt.Errorf("signingkeys: 解析 CA 私钥: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("signingkeys: CA 私钥不是 ECDSA")
	}
	cert, err := parseCertPEM(*k.CertPEM)
	if err != nil {
		return nil, nil, err
	}
	return key, cert, nil
}

// ---- 初始化 ----

// InitOptions 是 Initialize 的参数。
type InitOptions struct {
	// ServerURL 是 Server 对节点可达的地址（安装向导采集）；其主机名/IP 写入叶证书 SAN。必填。
	ServerURL string
	// CAValidityYears 是 CA 有效期（年）；0 = 默认 10；范围 5–10，超出拒绝（不静默截断）。
	CAValidityYears int
	// Actor 是审计操作者；nil = 系统组件 "setup"（安装向导/CLI 没有登录主体）。
	Actor *audit.Actor
}

// InitResult 是一次初始化的结果。
type InitResult struct {
	// Created 是本次新建的 purpose；全部已存在（幂等）时为空。
	Created []Purpose
	// LineageInitialized 报告本次是否写入了 deploy.lineage=1（已存在则不改，为 false）。
	LineageInitialized bool
	// Keys 是三种 purpose 当前 active 密钥的公开信息。
	Keys []KeyInfo
}

// CAFingerprint 返回结果里 CA 证书的指纹（enroll Token 的 ca_fp）。
func (r InitResult) CAFingerprint() string {
	for _, k := range r.Keys {
		if k.Purpose == PurposeMTLSCA {
			return k.Fingerprint
		}
	}
	return ""
}

// Initialize 完成信任域②③的密钥初始化（AC1）：生成 Ed25519 配置签名密钥、ECDSA P-256 私有 CA（默认 10 年）与由该 CA 签发的 Server gRPC 叶证书
// （serverAuth，SAN = server_url 主机 + 设置项 grpc.tls.san_extra，90 天），私钥经信封加密（AAD=表|列|uid）入库，并初始化 system_settings(deploy.lineage)=1。
//
// 约束：
//   - 单事务：密钥入库、lineage 与审计（audit.Record）同一事务，审计写入失败整体回滚、无残留（fail-closed）；
//   - 幂等：某 purpose 已有 active 行则不覆盖、不重新生成；deploy.lineage 已存在则不改；全部已存在时不写审计；
//   - 并发安全：每个 purpose 至多一个 active 由唯一索引保证；并发初始化时后到者遇重复键/死锁整体重试，重试后看到已提交的结果；恰有一个调用报告 Created；
//   - 缺主密钥时拒绝：先做一次真实的加解密自检，失败返回 ErrMasterKeyMissing，不写入任何东西；
//   - 只用 app 池（权限核对见 repository 包注释）。调用者（Story 1.24 安装向导）以本函数为入口；本 Story 不注册 setup 子命令。
//
// 签发的叶证书 SAN 在初始化时确定；之后改 server_url/grpc.tls.san_extra 不会改动已存在的叶证书（热重载与 server.cert_rotate 轮换属 Story 3.10）。
func (s *Service) Initialize(ctx context.Context, o InitOptions) (InitResult, error) {
	years := o.CAValidityYears
	if years == 0 {
		years = DefaultCAYears
	}
	if years < MinCAYears || years > MaxCAYears {
		return InitResult{}, ErrInvalidCAValidity
	}
	host, err := serverHost(o.ServerURL)
	if err != nil {
		return InitResult{}, err
	}
	if err := s.preflight(ctx); err != nil {
		return InitResult{}, err
	}
	actor := audit.SystemActor("setup")
	if o.Actor != nil {
		actor = *o.Actor
	}

	var res InitResult
	for attempt := 0; ; attempt++ {
		res = InitResult{}
		err = s.pool.WithTx(ctx, func(ctx context.Context) error { return s.initTx(ctx, host, years, actor, &res) })
		if err == nil {
			return res, nil
		}
		if attempt+1 >= maxInitRetries || !retryable(err) {
			return InitResult{}, err
		}
		select { // 重复键/死锁：并发初始化的败者稍等后整体重来，重试时会看到胜者已提交的行
		case <-ctx.Done():
			return InitResult{}, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 10 * time.Millisecond):
		}
	}
}

func retryable(err error) bool {
	if db.IsDuplicate(err) {
		return true
	}
	n, ok := db.MySQLErrorNumber(err)
	return ok && (n == 1213 || n == 1205) // ER_LOCK_DEADLOCK / ER_LOCK_WAIT_TIMEOUT
}

// preflight 用 KEK 做一次真实的加解密往返：主密钥缺失/已关闭/不可用时拒绝初始化。
func (s *Service) preflight(ctx context.Context) error {
	if s.box == nil || s.box.CurrentKeyID() == "" {
		return ErrMasterKeyMissing
	}
	probe := make([]byte, 32)
	defer secretbox.Zero(probe)
	if _, err := rand.Read(probe); err != nil {
		return fmt.Errorf("signingkeys: 自检随机数: %w", err)
	}
	rc := secretbox.RecordContext{Table: tableName, Column: columnName, UID: "preflight"}
	enc, err := s.box.Seal(ctx, probe, rc)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMasterKeyMissing, err)
	}
	pt, err := s.box.Open(ctx, enc, rc)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMasterKeyMissing, err)
	}
	defer secretbox.Zero(pt)
	return nil
}

// serverHost 从 server_url 取主机名/IP（规范化）。
func serverHost(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", ErrInvalidServerURL
	}
	h, ok := sysconfig.NormalizeHost(u.Hostname())
	if !ok {
		return "", ErrInvalidServerURL
	}
	return h, nil
}

func (s *Service) sanExtra(ctx context.Context) ([]string, error) {
	raw, err := s.repo.GetSettingJSON(ctx, sysconfig.KeyGRPCSANExtra)
	if err != nil {
		return nil, fmt.Errorf("signingkeys: 读取 %s: %w", sysconfig.KeyGRPCSANExtra, err)
	}
	if raw == nil {
		return nil, nil
	}
	def, _ := sysconfig.Lookup(sysconfig.KeyGRPCSANExtra)
	norm, verr := def.Normalize(json.RawMessage(*raw))
	if verr != nil {
		return nil, fmt.Errorf("signingkeys: %s 的取值非法: %w", sysconfig.KeyGRPCSANExtra, verr)
	}
	var list []string
	if len(norm) > 0 {
		if err := json.Unmarshal(norm, &list); err != nil {
			return nil, fmt.Errorf("signingkeys: %s 无法解析: %w", sysconfig.KeyGRPCSANExtra, err)
		}
	}
	return list, nil
}

func (s *Service) initTx(ctx context.Context, host string, years int, actor audit.Actor, res *InitResult) error {
	now := s.clk.Now()
	sc := s.newScratch()
	defer sc.wipe()

	// ① 配置签名密钥（信任域②）。
	cfg, cfgOK, err := s.repo.GetActive(ctx, string(PurposeConfigSigning))
	if err != nil {
		return err
	}
	if !cfgOK {
		if cfg, err = s.createConfigKey(ctx, sc, now); err != nil {
			return err
		}
		res.Created = append(res.Created, PurposeConfigSigning)
	}

	// ② 私有 CA（信任域③）。
	ca, caOK, err := s.repo.GetActive(ctx, string(PurposeMTLSCA))
	if err != nil {
		return err
	}
	var caKey *ecdsa.PrivateKey
	var caCert *x509.Certificate
	if !caOK {
		if ca, caKey, caCert, err = s.createCA(ctx, sc, now, years); err != nil {
			return err
		}
		res.Created = append(res.Created, PurposeMTLSCA)
	}

	// ③ Server gRPC 叶证书（由该 CA 签发）。
	srv, srvOK, err := s.repo.GetActive(ctx, string(PurposeGRPCServer))
	if err != nil {
		return err
	}
	if !srvOK {
		extra, err := s.sanExtra(ctx)
		if err != nil {
			return err
		}
		if caKey == nil { // CA 已存在（先前部分初始化或叶证书被移除）：解密 CA 私钥仅用于这一次签发
			if caKey, caCert, err = s.openCA(ctx, sc, ca); err != nil {
				return err
			}
		}
		if srv, err = s.createLeaf(ctx, sc, now, caCert, caKey, sanList(host, extra)); err != nil {
			return err
		}
		res.Created = append(res.Created, PurposeGRPCServer)
	}

	// ④ deploy.lineage=1（已存在则不改）。
	uid, err := s.ids.New()
	if err != nil {
		return fmt.Errorf("signingkeys: 生成 uid: %w", err)
	}
	if res.LineageInitialized, err = s.repo.EnsureSetting(ctx, uid, lineageKey, lineageGroup, initialLineage); err != nil {
		return fmt.Errorf("signingkeys: 初始化 %s: %w", lineageKey, err)
	}

	// ⑤ 审计（同一事务；失败整体回滚）。全部已存在 = 没有任何变更，不写审计。
	for _, k := range []repository.Key{cfg, ca, srv} {
		in, err := toInfo(k)
		if err != nil {
			return err
		}
		res.Keys = append(res.Keys, in)
	}
	if len(res.Created) == 0 && !res.LineageInitialized {
		return nil
	}
	created := make([]string, len(res.Created))
	for i, p := range res.Created {
		created[i] = string(p)
	}
	fps := map[string]string{}
	for _, k := range res.Keys {
		fps[string(k.Purpose)] = k.Fingerprint // 公开指纹，不含任何私钥材料
	}
	return s.audit.Record(ctx, audit.ActionSigningKeysInit, audit.Event{
		ResourceType: audit.ResourceSigningKey, ResourceUID: "initialize", Actor: &actor,
		Changes: map[string]any{"created": created, "lineage_initialized": res.LineageInitialized, "fingerprints": fps, "ca_validity_years": years},
	})
}

func sanList(host string, extra []string) []string {
	seen := map[string]bool{host: true}
	out := []string{host}
	for _, e := range extra {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

func (s *Service) seal(ctx context.Context, plaintext []byte, uid string) (string, error) {
	enc, err := s.box.Seal(ctx, plaintext, secretbox.RecordContext{Table: tableName, Column: columnName, UID: uid})
	if err != nil {
		return "", fmt.Errorf("signingkeys: 加密私钥: %w", err)
	}
	return enc, nil
}

func keyID(prefix string, publicKey []byte) string {
	return prefix + signing.Fingerprint(publicKey)[:16]
}

func (s *Service) createConfigKey(ctx context.Context, sc *scratch, now time.Time) (repository.Key, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return repository.Key{}, fmt.Errorf("signingkeys: 生成 Ed25519 密钥: %w", err)
	}
	sc.hold(string(PurposeConfigSigning)+".ed25519", priv)
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return repository.Key{}, err
	}
	sc.hold(string(PurposeConfigSigning)+".pkcs8", der)
	uid, err := s.ids.New()
	if err != nil {
		return repository.Key{}, fmt.Errorf("signingkeys: 生成 uid: %w", err)
	}
	enc, err := s.seal(ctx, der, uid)
	if err != nil {
		return repository.Key{}, err
	}
	act := now
	k := repository.Key{UID: uid, Purpose: string(PurposeConfigSigning), KeyID: keyID("cfg-", pub), Algorithm: AlgEd25519, PublicKey: pub,
		PrivateKeyEnc: enc, EncKeyID: s.box.CurrentKeyID(), Status: StatusActive, ActivatedAt: &act}
	if err := s.repo.Insert(ctx, k); err != nil {
		return repository.Key{}, err
	}
	return k, nil
}

func (s *Service) createCA(ctx context.Context, sc *scratch, now time.Time, years int) (repository.Key, *ecdsa.PrivateKey, *x509.Certificate, error) {
	key, cert, der, err := newCA(now, years)
	if err != nil {
		return repository.Key{}, nil, nil, err
	}
	k, err := s.insertECDSA(ctx, sc, now, PurposeMTLSCA, "ca-", key, der)
	return k, key, cert, err
}

func (s *Service) createLeaf(ctx context.Context, sc *scratch, now time.Time, ca *x509.Certificate, caKey *ecdsa.PrivateKey, hosts []string) (repository.Key, error) {
	key, _, der, err := newServerLeaf(now, ca, caKey, hosts)
	if err != nil {
		return repository.Key{}, err
	}
	return s.insertECDSA(ctx, sc, now, PurposeGRPCServer, "srv-", key, der)
}

func (s *Service) insertECDSA(ctx context.Context, sc *scratch, now time.Time, p Purpose, prefix string, key *ecdsa.PrivateKey, certDER []byte) (repository.Key, error) {
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return repository.Key{}, err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return repository.Key{}, err
	}
	sc.hold(string(p)+".pkcs8", pkcs8)
	uid, err := s.ids.New()
	if err != nil {
		return repository.Key{}, fmt.Errorf("signingkeys: 生成 uid: %w", err)
	}
	enc, err := s.seal(ctx, pkcs8, uid)
	if err != nil {
		return repository.Key{}, err
	}
	pemText := certPEM(certDER)
	act := now
	k := repository.Key{UID: uid, Purpose: string(p), KeyID: keyID(prefix, spki), Algorithm: AlgP256, PublicKey: spki, CertPEM: &pemText,
		PrivateKeyEnc: enc, EncKeyID: s.box.CurrentKeyID(), Status: StatusActive, ActivatedAt: &act}
	if err := s.repo.Insert(ctx, k); err != nil {
		return repository.Key{}, err
	}
	return k, nil
}
