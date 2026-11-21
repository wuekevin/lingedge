package sysconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/audit"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/internal/platform/sysconfig/repository"
	"github.com/wuekevin/lingedge/pkg/secretbox"
	"github.com/wuekevin/lingedge/pkg/version"
)

// DefaultCacheTTL 是设置快照缓存的存活时间：本实例写入后立即失效，其他实例最迟此时间内读到新值（运行模式切换「无需重启」）。
const DefaultCacheTTL = 5 * time.Second

// IDGenerator 生成 system_settings.uid。
type IDGenerator interface{ New() (string, error) }

// Options 是 New 的参数。
type Options struct {
	// Pool 必须是 app 连接池。
	Pool  db.Pool
	Clock clock.Clock
	IDs   IDGenerator
	Log   *zap.Logger
	// Box 是信封加密（敏感键经它加密落 value_enc，AAD = system_settings|value_enc|<uid>）；非 ManifestOnly 时必填。
	Box *secretbox.Box
	// Audit 是审计记录器：设置变更与审计行同一事务（fail-closed，IMP-19）；非 ManifestOnly 时必填。
	Audit audit.Recorder
	// Migrations 是迁移版本探针（httpx.MigrationProbeFromApp，复用 Story 1.13）；/admin/system/version 据此返回真实迁移版本。
	Migrations httpx.MigrationProbe
	// Redis 用于公开接口的按 IP 限流（不可用时 redisx 自动降级到进程内计数）；nil = 不限流（仅测试）。
	Redis *redisx.Client
	// CacheTTL <= 0 取 DefaultCacheTTL。
	CacheTTL time.Duration
	// PublicRateLimit 是公开接口每 IP 每分钟的请求上限；<=0 取 120 [ASSUMPTION]。
	PublicRateLimit int
	// ManifestOnly 仅用于导出路由清单（lingedge-server routes）。
	ManifestOnly bool
}

// Service 是类型化的 system_settings 服务。
type Service struct {
	repo   *repository.Repo
	pool   db.Pool
	clk    clock.Clock
	ids    IDGenerator
	log    *zap.Logger
	box    *secretbox.Box
	audit  audit.Recorder
	probe  httpx.MigrationProbe
	cache  time.Duration
	limit  *redisx.Limiter
	ks     redisx.Keyspace
	pubMax int

	mu      sync.Mutex
	snap    snapshot
	have    bool
	expires time.Time
	gen     uint64 // 每次失效 +1：加载期间发生了失效则不回填过期结果
}

// snapshot 是非敏感设置的内存快照（键 → 规范 JSON）。
type snapshot struct {
	vals map[string]json.RawMessage
}

func newService(o Options) (*Service, error) {
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	if o.Log == nil {
		o.Log = zap.NewNop()
	}
	if o.CacheTTL <= 0 {
		o.CacheTTL = DefaultCacheTTL
	}
	if o.PublicRateLimit <= 0 {
		o.PublicRateLimit = 120
	}
	s := &Service{clk: o.Clock, log: o.Log.Named("sysconfig"), cache: o.CacheTTL, probe: o.Migrations, pubMax: o.PublicRateLimit}
	if o.ManifestOnly {
		return s, nil
	}
	if o.Pool == nil || o.IDs == nil || o.Box == nil || o.Audit == nil {
		return nil, errors.New("sysconfig: 缺少必需依赖（Pool、IDs、Box、Audit）")
	}
	repo, err := repository.New(o.Pool)
	if err != nil {
		return nil, err
	}
	s.repo, s.pool, s.ids, s.box, s.audit = repo, o.Pool, o.IDs, o.Box, o.Audit
	if o.Redis != nil {
		s.limit, s.ks = redisx.NewLimiter(o.Redis, nil), o.Redis.Keyspace()
	}
	return s, nil
}

// ---- 读取（缓存快照）----

func (s *Service) loadSnapshot(ctx context.Context) (snapshot, error) {
	now := s.clk.Now()
	s.mu.Lock()
	if s.have && now.Before(s.expires) {
		v := s.snap
		s.mu.Unlock()
		return v, nil
	}
	gen := s.gen
	s.mu.Unlock()

	rows, err := s.repo.ListSettings(ctx)
	if err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.have { // 读取故障：沿用上一次的值并告警（运行模式的最后已知值；从未成功读取过则失败，由调用方 fail-closed）
			s.log.Warn("读取系统设置失败：沿用上一次快照", zap.Error(err))
			s.expires = now.Add(2 * time.Second)
			return s.snap, nil
		}
		return snapshot{}, err
	}
	next := snapshot{vals: make(map[string]json.RawMessage, len(rows))}
	for _, r := range rows {
		d, known := Lookup(r.Key)
		if !known || d.Sensitive || r.ValueJSON == nil {
			continue // 未知键、敏感键（不缓存）或密文行不进入快照
		}
		next.vals[r.Key] = json.RawMessage(*r.ValueJSON)
	}
	s.mu.Lock()
	if s.gen == gen { // 加载期间发生过失效：不回填，下次重新读取
		s.snap, s.have, s.expires = next, true, now.Add(s.cache)
	}
	s.mu.Unlock()
	return next, nil
}

func (s *Service) invalidate() {
	s.mu.Lock()
	s.gen++
	s.have = false
	s.mu.Unlock()
}

// value 返回键的有效取值（已存值经注册表重新校验：库里被带外写入的非法值回退默认并告警，不会让运行模式等关键值失真）。
func (sn snapshot) value(log *zap.Logger, key string) any {
	d, ok := Lookup(key)
	if !ok {
		return nil
	}
	if isCapability(key) {
		return StatusUnverified // 能力状态恒为 unverified：库里被带外写入的值不生效（NFR-27）
	}
	if raw, present := sn.vals[key]; present {
		if norm, verr := d.Normalize(raw); verr == nil && norm != nil {
			var v any
			dec := json.NewDecoder(bytes.NewReader(norm))
			dec.UseNumber()
			if dec.Decode(&v) == nil {
				return v
			}
		}
		log.Warn("系统设置取值无效，使用默认值", zap.String("key", key))
	}
	return d.Default
}

func isCapability(key string) bool {
	for _, c := range AdvancedCapabilities {
		if key == capabilityKey(c) {
			return true
		}
	}
	return false
}

func (sn snapshot) str(log *zap.Logger, key string) string {
	v, _ := sn.value(log, key).(string)
	return v
}

func (sn snapshot) strPtr(log *zap.Logger, key string) *string {
	if v, ok := sn.value(log, key).(string); ok && v != "" {
		return &v
	}
	return nil
}

func (sn snapshot) boolean(log *zap.Logger, key string) bool {
	v, _ := sn.value(log, key).(bool)
	return v
}

// RunMode 返回当前运行模式（缺省 self_hosted）。
func (s *Service) RunMode(ctx context.Context) (string, error) {
	sn, err := s.loadSnapshot(ctx)
	if err != nil {
		return "", err
	}
	m := sn.str(s.log, KeyRunMode)
	if m != RunModeSelfHosted && m != RunModeCommercial {
		return DefaultRunMode, nil
	}
	return m, nil
}

// Features 返回 /auth/me 的 features（运行模式 + 已交付模块 + 能力状态）。
func (s *Service) Features(ctx context.Context) (FeaturesView, error) {
	m, err := s.RunMode(ctx)
	if err != nil {
		return FeaturesView{}, err
	}
	return FeaturesView{RunMode: m, ModulesDelivered: ModulesDelivered(), CapabilityStatus: CapabilityStatus()}, nil
}

// Enabled 实现 httpx.FeatureGate。
func (s *Service) Enabled(ctx context.Context, feature string) (bool, error) {
	m, err := s.RunMode(ctx)
	if err != nil {
		return false, err
	}
	return FeatureEnabled(m, feature), nil
}

// ---- 设置视图（管理员读取，直读数据库，保证读到自己刚写入的值）----

// SettingsView 返回 group（空 = 全部）内全部已注册键的有效取值：未设置 = 默认值；敏感键一律脱敏为 {"configured": bool}；能力状态恒为 unverified。
func (s *Service) SettingsView(ctx context.Context, group string) (map[string]any, error) {
	if group != "" && !contains(Groups, group) {
		return nil, apierr.Validation(apierr.Detail{Field: "group", Reason: "unknown group"})
	}
	rows, err := s.repo.ListSettings(ctx)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	return s.view(group, rows), nil
}

func (s *Service) view(group string, rows []repository.Setting) map[string]any {
	by := make(map[string]repository.Setting, len(rows))
	for _, r := range rows {
		by[r.Key] = r
	}
	out := map[string]any{}
	for _, d := range defs {
		if group != "" && d.Group != group {
			continue
		}
		switch {
		case d.Sensitive:
			r, ok := by[d.Key]
			out[d.Key] = map[string]bool{"configured": ok && r.ValueEnc != nil}
		case isCapability(d.Key):
			out[d.Key] = StatusUnverified
		default:
			sn := snapshot{vals: map[string]json.RawMessage{}}
			if r, ok := by[d.Key]; ok && r.ValueJSON != nil {
				sn.vals[d.Key] = json.RawMessage(*r.ValueJSON)
			}
			out[d.Key] = sn.value(s.log, d.Key)
		}
	}
	return out
}

// ---- 写入 ----

// change 是一个键的变更：Norm == nil 表示恢复默认（删除行）。
type change struct {
	def  Def
	norm json.RawMessage
	// secret 是敏感键的明文（只在内存里短暂存在，加密后丢弃）。
	secret string
}

// Patch 批量更新（通用 PATCH /admin/settings）：只接受 Patchable 的键，整批校验，任一不合法则整批 400 且什么都不写。
func (s *Service) Patch(ctx context.Context, values map[string]json.RawMessage) (map[string]any, error) {
	if len(values) == 0 || len(values) > 64 {
		return nil, apierr.Validation(apierr.Detail{Field: "values", Reason: "must contain 1-64 keys"})
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var details []apierr.Detail
	var changes []change
	for _, k := range keys {
		d, ok := Lookup(k)
		field := "values." + k
		switch {
		case !ok:
			details = append(details, apierr.Detail{Field: field, Reason: "unknown setting key"})
			continue
		case isCapability(k):
			// NFR-27：能力状态由独立技术验证决定，不能经设置写入（尤其不能置为 available）。
			details = append(details, apierr.Detail{Field: field, Reason: "capability status is unverified until independently verified and cannot be set (NFR-27)"})
			continue
		case !d.Patchable:
			details = append(details, apierr.Detail{Field: field, Reason: "cannot be changed here; use " + d.Dedicated})
			continue
		}
		ch, verr := s.prepare(d, values[k])
		if verr != nil {
			details = append(details, apierr.Detail{Field: field, Reason: verr.Reason})
			continue
		}
		changes = append(changes, ch)
	}
	if len(details) > 0 {
		return nil, apierr.Validation(details...)
	}
	if err := s.applyCrossField(ctx, changes); err != nil {
		return nil, err
	}
	if err := s.apply(ctx, func(ctx context.Context, ev audit.Event) error {
		return s.audit.Record(ctx, audit.ActionSettingUpdate, ev)
	}, audit.ResourceSetting, "", changes, nil); err != nil {
		return nil, err
	}
	return s.SettingsView(ctx, "")
}

func (s *Service) prepare(d Def, raw json.RawMessage) (change, *ValidationError) {
	norm, verr := d.Normalize(raw)
	if verr != nil {
		return change{}, verr
	}
	ch := change{def: d, norm: norm}
	if d.Sensitive && norm != nil {
		var str string
		if json.Unmarshal(norm, &str) != nil || str == "" {
			return change{}, bad(d.Key, "must be a non-empty string (use null to clear)")
		}
		ch.secret = str
		ch.norm = nil // 明文不进入变更摘要，也不会被当作 value_json
		ch.def = d
	}
	return ch, nil
}

// applyCrossField 校验跨键约束（以「库里现值 + 本次变更」为准）：滑动过期 ≤ 绝对过期；API Token 默认有效期 ≤ 上限。
func (s *Service) applyCrossField(ctx context.Context, changes []change) error {
	rows, err := s.repo.ListSettings(ctx)
	if err != nil {
		return apierr.Internal(err)
	}
	sn := snapshot{vals: map[string]json.RawMessage{}}
	for _, r := range rows {
		if r.ValueJSON != nil {
			sn.vals[r.Key] = json.RawMessage(*r.ValueJSON)
		}
	}
	for _, c := range changes {
		if c.norm != nil {
			sn.vals[c.def.Key] = c.norm
		} else {
			delete(sn.vals, c.def.Key)
		}
	}
	num := func(k string) int64 { n, _ := sn.value(s.log, k).(json.Number); i, _ := n.Int64(); return i }
	var details []apierr.Detail
	touched := func(k string) bool {
		for _, c := range changes {
			if c.def.Key == k {
				return true
			}
		}
		return false
	}
	if (touched(KeySessionIdleTTL) || touched(KeySessionAbsoluteTTL)) && num(KeySessionIdleTTL) > num(KeySessionAbsoluteTTL) {
		details = append(details, apierr.Detail{Field: "values." + KeySessionIdleTTL, Reason: "must not exceed " + KeySessionAbsoluteTTL})
	}
	if (touched(KeyAPITokenDefaultTTL) || touched(KeyAPITokenMaxTTL)) && num(KeyAPITokenDefaultTTL) > num(KeyAPITokenMaxTTL) {
		details = append(details, apierr.Detail{Field: "values." + KeyAPITokenDefaultTTL, Reason: "must not exceed " + KeyAPITokenMaxTTL})
	}
	if len(details) > 0 {
		return apierr.Validation(details...)
	}
	return nil
}

func retryable(err error) bool {
	n, ok := db.MySQLErrorNumber(err)
	return ok && (n == 1213 || n == 1205 || n == db.MySQLErrDupEntry) // 死锁 / 锁等待超时 / 并发首次插入同一键
}

// tx 在事务内执行 fn；并发写同一键引起的死锁/重复键会整体重试（事务已回滚，审计行不会重复）。
func (s *Service) tx(ctx context.Context, fn func(ctx context.Context) error) error {
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		if err = s.pool.WithTx(ctx, fn); err == nil || !retryable(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 15 * time.Millisecond):
		}
	}
	return err
}

// recordFn 以调用点固定的审计动作记录事件（动作必须是 audit.Action* 常量，门禁 audit-check 据此核对 Catalog 与 .Record 调用点）。
type recordFn func(ctx context.Context, ev audit.Event) error

// apply 在**同一事务**里写入变更并记录审计（审计失败 = 整体回滚，fail-closed）。extra 是附加的审计摘要字段（如运行模式的 from/to）。
func (s *Service) apply(ctx context.Context, record recordFn, resType, resUID string, changes []change, extra map[string]any) error {
	var actor *uint64
	if p, ok := httpx.PrincipalFrom(ctx); ok {
		if id := p.ActorID(); id > 0 {
			actor = &id
		}
	}
	err := s.tx(ctx, func(ctx context.Context) error {
		summary := map[string]any{}
		for _, c := range changes {
			ch, err := s.applyOne(ctx, c, actor)
			if err != nil {
				return err
			}
			summary[c.def.Key] = ch
		}
		ev := audit.Event{ResourceType: resType, ResourceUID: resUID, Changes: map[string]any{"settings": summary}}
		for k, v := range extra {
			ev.Changes[k] = v
		}
		return record(ctx, ev)
	})
	if err != nil {
		var ae *apierr.Error
		if errors.As(err, &ae) {
			return err
		}
		return apierr.Internal(err)
	}
	s.invalidate()
	return nil
}

// applyOne 写入一个键，返回审计摘要项（敏感键只记「已变更」，不含任何取值）。
func (s *Service) applyOne(ctx context.Context, c change, actor *uint64) (any, error) {
	cur, exists, err := s.repo.GetSettingForUpdate(ctx, c.def.Key)
	if err != nil {
		return nil, fmt.Errorf("读取设置 %s: %w", c.def.Key, err)
	}
	if c.def.Sensitive {
		if c.secret == "" { // 清除
			if exists {
				if err := s.repo.DeleteSetting(ctx, cur.ID); err != nil {
					return nil, err
				}
			}
			return map[string]any{"changed": exists, "configured": false}, nil
		}
		id := cur.UID
		if !exists {
			if id, err = s.ids.New(); err != nil {
				return nil, err
			}
		}
		enc, err := s.box.Seal(ctx, []byte(c.secret), secretbox.RecordContext{Table: "system_settings", Column: "value_enc", UID: id})
		if err != nil {
			return nil, fmt.Errorf("加密设置 %s: %w", c.def.Key, err)
		}
		if exists {
			err = s.repo.UpdateSetting(ctx, cur.ID, nil, &enc, actor)
		} else {
			err = s.repo.InsertSetting(ctx, id, c.def.Key, c.def.Group, nil, &enc, actor)
		}
		return map[string]any{"changed": true, "configured": true}, err
	}
	var from, to any // 审计摘要用普通值（不放 json.RawMessage：脱敏器按类型遍历）
	if exists && cur.ValueJSON != nil {
		_ = json.Unmarshal([]byte(*cur.ValueJSON), &from)
	}
	if c.norm != nil {
		_ = json.Unmarshal(c.norm, &to)
	}
	switch {
	case c.norm == nil: // 恢复默认
		if exists {
			if err := s.repo.DeleteSetting(ctx, cur.ID); err != nil {
				return nil, err
			}
		}
	case exists:
		v := string(c.norm)
		err = s.repo.UpdateSetting(ctx, cur.ID, &v, nil, actor)
	default:
		id, ierr := s.ids.New()
		if ierr != nil {
			return nil, ierr
		}
		v := string(c.norm)
		err = s.repo.InsertSetting(ctx, id, c.def.Key, c.def.Group, &v, nil, actor)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"from": from, "to": to}, nil
}

// OpenSecret 解密敏感键的当前值（供邮件等内部消费者使用；不经任何 API 返回）。未配置返回 ("", false, nil)。
func (s *Service) OpenSecret(ctx context.Context, key string) (string, bool, error) {
	d, ok := Lookup(key)
	if !ok || !d.Sensitive {
		return "", false, fmt.Errorf("sysconfig: %q 不是敏感设置键", key)
	}
	rows, err := s.repo.ListSettings(ctx)
	if err != nil {
		return "", false, err
	}
	for _, r := range rows {
		if r.Key == key && r.ValueEnc != nil {
			b, err := s.box.Open(ctx, *r.ValueEnc, secretbox.RecordContext{Table: "system_settings", Column: "value_enc", UID: r.UID})
			if err != nil {
				return "", false, err
			}
			return string(b), true, nil
		}
	}
	return "", false, nil
}

// ---- 品牌 ----

// Brand 是 OpenAPI BrandSettings。
type Brand struct {
	Name            string  `json:"name"`
	NameEN          string  `json:"name_en"`
	LogoMediaUID    *string `json:"logo_media_uid"`
	FaviconMediaUID *string `json:"favicon_media_uid"`
	PrimaryColor    string  `json:"primary_color"`
	SupportEmail    string  `json:"support_email"`
}

func (sn snapshot) brand(log *zap.Logger) Brand {
	return Brand{Name: sn.str(log, KeyBrandName), NameEN: sn.str(log, KeyBrandNameEN), LogoMediaUID: sn.strPtr(log, KeyBrandLogoUID),
		FaviconMediaUID: sn.strPtr(log, KeyBrandFaviconUID), PrimaryColor: sn.str(log, KeyBrandPrimaryColor), SupportEmail: sn.str(log, KeyBrandSupportEmail)}
}

// Brand 读取品牌（Admin 与官网读同一份，FR-36）。fresh = true 直读数据库（管理员读取）；否则走快照缓存（公开接口）。
func (s *Service) Brand(ctx context.Context, fresh bool) (Brand, error) {
	if fresh {
		rows, err := s.repo.ListSettings(ctx)
		if err != nil {
			return Brand{}, apierr.Internal(err)
		}
		sn := snapshot{vals: map[string]json.RawMessage{}}
		for _, r := range rows {
			if r.ValueJSON != nil {
				sn.vals[r.Key] = json.RawMessage(*r.ValueJSON)
			}
		}
		return sn.brand(s.log), nil
	}
	sn, err := s.loadSnapshot(ctx)
	if err != nil {
		return Brand{}, apierr.Internal(err)
	}
	return sn.brand(s.log), nil
}

// PutBrand 整体替换品牌：请求里省略/为 null 的字段恢复默认。校验全部通过才写入（一个事务、一条审计）。
func (s *Service) PutBrand(ctx context.Context, in map[string]json.RawMessage) (Brand, error) {
	fields := map[string]string{"name": KeyBrandName, "name_en": KeyBrandNameEN, "logo_media_uid": KeyBrandLogoUID, "favicon_media_uid": KeyBrandFaviconUID,
		"primary_color": KeyBrandPrimaryColor, "support_email": KeyBrandSupportEmail}
	var details []apierr.Detail
	for f := range in {
		if _, ok := fields[f]; !ok {
			details = append(details, apierr.Detail{Field: f, Reason: "unknown field"})
		}
	}
	names := make([]string, 0, len(fields))
	for f := range fields {
		names = append(names, f)
	}
	sort.Strings(names)
	var changes []change
	for _, f := range names {
		d, _ := Lookup(fields[f])
		ch, verr := s.prepare(d, in[f])
		if verr != nil {
			details = append(details, apierr.Detail{Field: f, Reason: verr.Reason})
			continue
		}
		changes = append(changes, ch)
	}
	if len(details) > 0 {
		return Brand{}, apierr.Validation(details...)
	}
	if err := s.apply(ctx, func(ctx context.Context, ev audit.Event) error {
		return s.audit.Record(ctx, audit.ActionSettingBrandUpdate, ev)
	}, audit.ResourceSetting, "brand", changes, nil); err != nil {
		return Brand{}, err
	}
	return s.Brand(ctx, true)
}

// ---- 运行模式 ----

// SetRunMode 切换运行模式（FR-24：无需迁移）。同一事务内：行锁读取旧值 → 写新值 → 审计（from/to）；并发切换被行锁串行化，审计链的 from 依次衔接。
func (s *Service) SetRunMode(ctx context.Context, mode string) (string, error) {
	d, _ := Lookup(KeyRunMode)
	norm, verr := d.Normalize(json.RawMessage(fmt.Sprintf("%q", mode)))
	if verr != nil || norm == nil {
		reason := "must be one of self_hosted, commercial"
		return "", apierr.Validation(apierr.Detail{Field: "run_mode", Reason: reason})
	}
	if err := s.apply(ctx, func(ctx context.Context, ev audit.Event) error {
		return s.audit.Record(ctx, audit.ActionRunModeUpdate, ev)
	}, audit.ResourceSystem, "run_mode", []change{{def: d, norm: norm}}, map[string]any{"run_mode": mode}); err != nil {
		return "", err
	}
	return mode, nil
}

// RunModeFresh 直读数据库的运行模式（管理员读取，保证读到自己刚切换的值）。
func (s *Service) RunModeFresh(ctx context.Context) (string, error) {
	rows, err := s.repo.ListSettings(ctx)
	if err != nil {
		return "", apierr.Internal(err)
	}
	for _, r := range rows {
		if r.Key == KeyRunMode && r.ValueJSON != nil {
			var m string
			if json.Unmarshal([]byte(*r.ValueJSON), &m) == nil && (m == RunModeSelfHosted || m == RunModeCommercial) {
				return m, nil
			}
		}
	}
	return DefaultRunMode, nil
}

// ---- SSRF 白名单 ----

// SSRFView 是 OpenAPI SsrfAllowlist。
type SSRFView struct {
	Entries   json.RawMessage `json:"entries"`
	UpdatedAt *string         `json:"updated_at,omitempty"`
	UpdatedBy *string         `json:"updated_by"`
}

// SSRF 读取平台 SSRF 白名单。
func (s *Service) SSRF(ctx context.Context) (SSRFView, error) {
	rows, err := s.repo.ListSettings(ctx)
	if err != nil {
		return SSRFView{}, apierr.Internal(err)
	}
	v := SSRFView{Entries: json.RawMessage("[]")}
	for _, r := range rows {
		if r.Key == KeySSRFAllowCIDRs && r.ValueJSON != nil {
			v.Entries = json.RawMessage(*r.ValueJSON)
			t := r.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z")
			v.UpdatedAt, v.UpdatedBy = &t, r.UpdatedByUID
		}
	}
	return v, nil
}

// PutSSRF 整体替换平台 SSRF 白名单（再认证由路由 x-reauth 保证；审计同事务）。
func (s *Service) PutSSRF(ctx context.Context, entries json.RawMessage) (SSRFView, error) {
	d, _ := Lookup(KeySSRFAllowCIDRs)
	norm, verr := d.Normalize(entries)
	if verr != nil {
		return SSRFView{}, apierr.Validation(apierr.Detail{Field: "entries", Reason: verr.Reason})
	}
	if err := s.apply(ctx, func(ctx context.Context, ev audit.Event) error {
		return s.audit.Record(ctx, audit.ActionSSRFAllowlistUpdate, ev)
	}, audit.ResourceSetting, "ssrf_allowlist", []change{{def: d, norm: norm}}, nil); err != nil {
		return SSRFView{}, err
	}
	return s.SSRF(ctx)
}

// ---- 版本与安装记录 ----

// VersionInfo 是 OpenAPI VersionInfo。
type VersionInfo struct {
	ServerVersion   string             `json:"server_version"`
	APIVersion      string             `json:"api_version"`
	Build           *string            `json:"build"`
	InstallationID  *string            `json:"installation_id,omitempty"`
	Components      []ComponentVersion `json:"components"`
	SchemaMigration *SchemaMigration   `json:"schema_migration,omitempty"`
}

// SchemaMigration 是迁移版本（来自迁移版本探针，只读 schema_migrations）。
type SchemaMigration struct {
	Current string `json:"current"`
	Pending int32  `json:"pending"`
}

// ComponentVersion 是 OpenAPI ComponentVersion。
type ComponentVersion struct {
	Component      string  `json:"component"`
	InstanceName   string  `json:"instance_name"`
	Host           string  `json:"host"`
	Version        string  `json:"version"`
	Channel        string  `json:"channel"`
	Status         string  `json:"status"`
	InstalledAt    string  `json:"installed_at"`
	LastUpgradedAt *string `json:"last_upgraded_at"`
}

// APIVersion 是 REST API 版本（openapi servers 前缀 /api/v1）。
const APIVersion = "v1"

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func toComponent(i repository.Installation) ComponentVersion {
	c := ComponentVersion{Component: i.Component, InstanceName: i.InstanceName, Host: i.Host, Version: i.Version, Channel: i.Channel, Status: i.Status, InstalledAt: ts(i.InstalledAt)}
	if i.LastUpgradedAt != nil {
		t := ts(*i.LastUpgradedAt)
		c.LastUpgradedAt = &t
	}
	return c
}

// Version 返回真实的 Server 版本（pkg/version，构建期注入）、迁移版本（探针）与已登记的组件实例。
func (s *Service) Version(ctx context.Context) (VersionInfo, error) {
	v := VersionInfo{ServerVersion: version.Version, APIVersion: APIVersion, Components: []ComponentVersion{}}
	if version.Commit != "" && version.Commit != "unknown" {
		c := version.Commit
		v.Build = &c
	}
	rows, _, err := s.repo.ListInstallations(ctx, 500, 0)
	if err != nil {
		return v, apierr.Internal(err)
	}
	for _, r := range rows {
		v.Components = append(v.Components, toComponent(r))
	}
	if id, err := s.repo.InstallationID(ctx); err != nil {
		return v, apierr.Internal(err)
	} else if id != "" {
		v.InstallationID = &id
	}
	if s.probe != nil {
		st, err := s.probe.MigrationState(ctx)
		if err != nil {
			return v, apierr.Internal(err)
		}
		v.SchemaMigration = &SchemaMigration{Current: fmt.Sprintf("%06d", st.Applied), Pending: int32(st.Pending)}
	}
	return v, nil
}

// Installations 分页返回安装记录。
func (s *Service) Installations(ctx context.Context, limit, offset int) ([]ComponentVersion, int64, error) {
	rows, total, err := s.repo.ListInstallations(ctx, limit, offset)
	if err != nil {
		return nil, 0, apierr.Internal(err)
	}
	out := make([]ComponentVersion, 0, len(rows))
	for _, r := range rows {
		out = append(out, toComponent(r))
	}
	return out, total, nil
}

// ---- 公开信息 ----

// SiteInfo 是 OpenAPI PublicSiteInfo（无敏感信息：只含品牌、运行模式与公开入口）。
type SiteInfo struct {
	Brand            Brand    `json:"brand"`
	RunMode          string   `json:"run_mode"`
	RegistrationOpen bool     `json:"registration_open"`
	ConsoleInstalled bool     `json:"console_installed"`
	AdminURL         *string  `json:"admin_url"`
	ConsoleURL       *string  `json:"console_url"`
	DocsURL          *string  `json:"docs_url"`
	StatusURL        *string  `json:"status_url"`
	Locales          []string `json:"locales"`
}

// PublicVersion 是 OpenAPI PublicVersion。
type PublicVersion struct {
	Version    string `json:"version"`
	APIVersion string `json:"api_version"`
	RunMode    string `json:"run_mode"`
}

// SiteInfo 返回官网运行时信息。注册是否开放 = 设置开启 ∧ 商业模式（自用模式下注册路由本身 403 feature.disabled）。
func (s *Service) SiteInfo(ctx context.Context) (SiteInfo, error) {
	sn, err := s.loadSnapshot(ctx)
	if err != nil {
		return SiteInfo{}, apierr.Internal(err)
	}
	mode := sn.str(s.log, KeyRunMode)
	if mode != RunModeCommercial {
		mode = RunModeSelfHosted
	}
	info := SiteInfo{Brand: sn.brand(s.log), RunMode: mode, RegistrationOpen: mode == RunModeCommercial && sn.boolean(s.log, KeyRegistrationOpen),
		AdminURL: sn.strPtr(s.log, KeyAdminURL), ConsoleURL: sn.strPtr(s.log, KeyConsoleURL), DocsURL: sn.strPtr(s.log, KeyDocsURL), StatusURL: sn.strPtr(s.log, KeyStatusURL)}
	if v, ok := sn.value(s.log, KeyLocales).([]any); ok {
		for _, e := range v {
			if l, ok := e.(string); ok {
				info.Locales = append(info.Locales, l)
			}
		}
	} else if l, ok := sn.value(s.log, KeyLocales).([]string); ok {
		info.Locales = l
	}
	if info.Locales == nil {
		info.Locales = defaultLocales
	}
	installed, err := s.repo.ConsoleInstalled(ctx)
	if err != nil {
		return SiteInfo{}, apierr.Internal(err)
	}
	info.ConsoleInstalled = installed || info.ConsoleURL != nil
	return info, nil
}

// PublicVersion 返回公开版本信息。
func (s *Service) PublicVersion(ctx context.Context) (PublicVersion, error) {
	m, err := s.RunMode(ctx)
	if err != nil {
		return PublicVersion{}, apierr.Internal(err)
	}
	return PublicVersion{Version: version.Version, APIVersion: APIVersion, RunMode: m}, nil
}

// allowPublic 是公开接口的按 IP 限流（固定窗口 1 分钟）。返回 (放行, 建议等待)。
func (s *Service) allowPublic(ctx context.Context, ip string) (bool, time.Duration) {
	if s.limit == nil {
		return true, 0
	}
	id := make([]byte, 0, len(ip))
	for _, c := range []byte(ip) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '=':
			id = append(id, c)
		default:
			id = append(id, '_') // IPv6 的冒号等
		}
	}
	if len(id) == 0 {
		id = []byte("unknown")
	}
	k, err := s.ks.Key("rl", "public", string(id))
	if err != nil {
		return true, 0
	}
	d, err := s.limit.Allow(ctx, k, s.pubMax, time.Minute)
	if err != nil {
		return true, 0
	}
	return d.Allowed, d.RetryAfter
}
