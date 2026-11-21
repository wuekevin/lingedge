package signingkeys

import (
	"encoding/base64"
	"errors"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

// MetaList 是 GET /admin/system/signing-keys 的路由元数据（与 openapi.yaml 的 x-permissions/x-tenant-scope/x-audit/x-phase 一一对应；
// system:read 已由迁移 000004 种子）。只读、平台范围；租户主体（Console 会话 401 / 租户令牌 403）由钩子与隔离矩阵骨架断言。
var MetaList = httpx.RouteMeta{OperationID: "listSigningKeys", Method: "GET", Path: "/admin/system/signing-keys", Auth: httpx.AuthRequired,
	Permissions: []string{"system:read"}, TenantScope: httpx.ScopePlatform, Phase: "M1"}

// Register 注册路由。
func (s *Service) Register(r *httpx.Router) error {
	if r == nil {
		return errors.New("signingkeys: Router 为空")
	}
	return r.Register(MetaList, s.list)
}

// View 是 OpenAPI SigningKey 的 JSON 形态：只有公钥与指纹，类型上就没有私钥/密文字段——私钥不可能经此结构泄露。
type View struct {
	KeyID       string  `json:"key_id"`
	Purpose     string  `json:"purpose"`
	Algorithm   string  `json:"algorithm"`
	Status      string  `json:"status"`
	PublicKey   string  `json:"public_key"` // 标准 base64：ed25519 为 32 字节原始公钥；ecdsa_p256 为 PKIX DER
	Fingerprint string  `json:"fingerprint"`
	NotAfter    *string `json:"not_after"`
	ActivatedAt *string `json:"activated_at"`
	RetireAfter *string `json:"retire_after"`
	RevokedAt   *string `json:"revoked_at"`
}

func tsp(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format("2006-01-02T15:04:05.000Z")
	return &s
}

// ToView 把公开信息转为响应形态。
func ToView(k KeyInfo) View {
	return View{KeyID: k.KeyID, Purpose: string(k.Purpose), Algorithm: k.Algorithm, Status: k.Status, PublicKey: base64.StdEncoding.EncodeToString(k.PublicKey),
		Fingerprint: k.Fingerprint, NotAfter: tsp(k.NotAfter), ActivatedAt: tsp(k.ActivatedAt), RetireAfter: tsp(k.RetireAfter), RevokedAt: tsp(k.RevokedAt)}
}

func (s *Service) list(c *gin.Context) {
	pg, err := httpx.ParsePage(c.Request.URL.Query())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	keys, total, err := s.List(c.Request.Context(), pg.Limit(), pg.Offset())
	if err != nil {
		httpx.Fail(c, apierr.Internal(err))
		return
	}
	items := make([]View, 0, len(keys))
	for _, k := range keys {
		items = append(items, ToView(k))
	}
	httpx.OK(c, httpx.NewPageResult(items, pg, total))
}
