package sysconfig

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/audit"
)

// InitialSettings 是安装向导「系统基础配置」一次性写入的值（Story 1.24、tools-design §13 步骤 6）。零值字段不写。
type InitialSettings struct {
	// RunMode：self_hosted（缺省）| commercial（D-3）。必填。
	RunMode string
	// BrandName/BrandNameEN：站点名称（中/英文），空 = 保持默认。
	BrandName, BrandNameEN string
	// BaseURL：Server 公开 URL（http/https，无内嵌凭据），空 = 不设置（密钥初始化步骤将因缺少 server_url 而拒绝）。
	BaseURL string
	// Timezone：IANA 时区名，空 = 保持默认 UTC。
	Timezone string
	// DefaultLocale：zh-CN | en，决定 general.locales 的首项，空 = 保持默认顺序。
	DefaultLocale string
}

// ApplyInitial 在**一个事务**里写入向导的基础配置并记录审计（动作 setup.system，Actor = 系统组件 setup）。校验全部通过才写入；
// 幂等（重复提交同样的值只是重写同样的值，向导各步可重入，ADR-026 §6）。不经再认证检查：向导运行在认证体系建立之前，由 setup token 保护。
func (s *Service) ApplyInitial(ctx context.Context, in InitialSettings) error {
	if s.repo == nil {
		return errors.New("sysconfig: 未装配（ManifestOnly）")
	}
	enc := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	type kv struct {
		key string
		raw json.RawMessage
	}
	list := []kv{{KeyRunMode, enc(in.RunMode)}}
	if in.BrandName != "" {
		list = append(list, kv{KeyBrandName, enc(in.BrandName)})
	}
	if in.BrandNameEN != "" {
		list = append(list, kv{KeyBrandNameEN, enc(in.BrandNameEN)})
	}
	if in.BaseURL != "" {
		list = append(list, kv{KeyBaseURL, enc(in.BaseURL)})
	}
	if in.Timezone != "" {
		list = append(list, kv{KeyTimezone, enc(in.Timezone)})
	}
	switch in.DefaultLocale {
	case "":
	case "zh-CN":
		list = append(list, kv{KeyLocales, enc([]string{"zh-CN", "en"})})
	case "en":
		list = append(list, kv{KeyLocales, enc([]string{"en", "zh-CN"})})
	default:
		return apierr.Validation(apierr.Detail{Field: "default_locale", Reason: "must be zh-CN or en"})
	}
	var details []apierr.Detail
	var changes []change
	for _, e := range list {
		d, _ := Lookup(e.key)
		ch, verr := s.prepare(d, e.raw)
		if verr != nil {
			field := map[string]string{KeyRunMode: "run_mode", KeyBrandName: "brand.name", KeyBrandNameEN: "brand.name_en", KeyBaseURL: "base_url", KeyTimezone: "timezone", KeyLocales: "default_locale"}[e.key]
			details = append(details, apierr.Detail{Field: field, Reason: verr.Reason})
			continue
		}
		changes = append(changes, ch)
	}
	if len(details) > 0 {
		return apierr.Validation(details...)
	}
	sys := audit.SystemActor("setup")
	return s.apply(ctx, func(ctx context.Context, ev audit.Event) error {
		ev.Actor = &sys
		return s.audit.Record(ctx, audit.ActionSetupSystem, ev)
	}, audit.ResourceSystem, "setup", changes, map[string]any{"run_mode": in.RunMode})
}
