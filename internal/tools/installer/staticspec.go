package installer

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// staticSpec 是静态前端（admin-web / console / docs / status / website static）共用的生产 ComponentSpec（Story 1.40）。
// 同一框架复用于五个组件：差别只在 staticProfiles（SPA 回落、API 反代范围、不可变缓存前缀）。没有测试替身成分：
//   - InitConfig 渲染 Nginx 站点片段与安全头文件（纯函数，输入全部是校验后的 --set），落盘由 installer 第 18 步统一负责（不覆盖、冲突写 .new）；
//   - Start 无服务可启动（静态前端由 Nginx 托管；可选的 conf.d 软链 + nginx -t && reload 在 installer 的第 19 步完成并带 journal 撤销）；
//   - Health 校验真实落盘的结果：current 指向本版本、public/index.html 是非空普通文件；
//   - ValidatePayload 在解压落地前校验包布局（payload/public/index.html、只含 public 与 share）。
type staticSpec struct{ name string }

func (s staticSpec) Name() string { return s.name }

// Executables：静态资源没有可执行文件。
func (staticSpec) Executables(version, variant string) []string { return nil }

// ValidateSettings 校验全部 --set（含必填的 host）。installer 对静态前端总会调用它，即使没有任何 --set。
func (s staticSpec) ValidateSettings(settings map[string]string) error {
	_, err := ParseStaticSettings(s.name, settings)
	return err
}

// InitConfig 返回站点片段与安全头文件。SSR 变体属 Story 2.14（Nuxt Nitro + systemd + revalidate 密钥），这里明确拒绝，不假装安装。
func (s staticSpec) InitConfig(_ context.Context, in ConfigInput) ([]ConfigFile, error) {
	if in.Variant == "ssr" {
		return nil, fmt.Errorf("website 的 ssr 变体由 Story 2.14 交付（systemd 单元、Node.js 依赖、revalidate 密钥）；本 Story 只安装静态托管形态")
	}
	set, err := ParseStaticSettings(s.name, in.Settings)
	if err != nil {
		return nil, err
	}
	conf, hdr, err := RenderStaticSite(SiteParams{Component: s.name, Host: set.Host, APIUpstream: set.APIUpstream, APICA: set.APICA,
		TLSCert: set.TLSCert, TLSKey: set.TLSKey, HTTPPort: set.HTTPPort, HTTPSPort: set.HTTPSPort, IPv6: set.IPv6})
	if err != nil {
		return nil, err
	}
	return []ConfigFile{{Path: StaticConfPath(s.name), Content: conf}, {Path: StaticHeadersPath(s.name), Content: hdr}}, nil
}

// Start：无服务。
func (staticSpec) Start(context.Context, *Runtime) error { return nil }

// Health 校验落盘结果：current → releases/<版本>，且 public/index.html 是非空普通文件。
func (s staticSpec) Health(_ context.Context, rt *Runtime) error {
	cur := filepath.Join(rt.Root, "/opt/lingedge/"+s.name+"/current")
	target, err := os.Readlink(cur)
	if err != nil {
		return fmt.Errorf("current 链接不可读：%w", err)
	}
	if target != "releases/"+rt.Version {
		return fmt.Errorf("current 指向 %q，期望 releases/%s", target, rt.Version)
	}
	st, err := os.Stat(filepath.Join(cur, "public", "index.html"))
	if err != nil {
		return fmt.Errorf("public/index.html 不可读：%w", err)
	}
	if !st.Mode().IsRegular() || st.Size() == 0 {
		return fmt.Errorf("public/index.html 不是非空普通文件")
	}
	return nil
}

// ValidatePayload 在解压落地前校验包布局（payloadDir 是已验签的 staging 下的 payload 目录）：
// 必须有 public/index.html；顶层只允许 public/（站点根）与 share/（流水线的 SBOM）——不接受把任意文件带进 /opt/lingedge/<c>/releases/<v>/。
func (staticSpec) ValidatePayload(payloadDir string) error {
	ents, err := os.ReadDir(payloadDir)
	if err != nil {
		return err
	}
	var extra []string
	for _, e := range ents {
		if (e.Name() == "public" || e.Name() == "share") && e.IsDir() {
			continue
		}
		extra = append(extra, e.Name())
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return fmt.Errorf("包布局不符：顶层只允许 public/ 与 share/，发现 %v", extra)
	}
	st, err := os.Lstat(filepath.Join(payloadDir, "public", "index.html"))
	if err != nil {
		return fmt.Errorf("包布局不符：缺少 public/index.html（%w）", err)
	}
	if st.Mode()&fs.ModeType != 0 || st.Size() == 0 {
		return fmt.Errorf("包布局不符：public/index.html 必须是非空普通文件")
	}
	return nil
}

// payloadValidator 是可选的组件接口：在解压落地前校验包布局（失败 → 第 14 步失败并回滚，不会落地任何文件）。
type payloadValidator interface{ ValidatePayload(payloadDir string) error }

// staticFrontend 标识静态前端规格（installer 对它们追加独立主机名校验、同域名站点冲突与 Nginx 软链决策）。
type staticFrontend interface{ isStaticFrontend() }

func (staticSpec) isStaticFrontend() {}

func staticSpecs() SpecSet {
	out := SpecSet{}
	for _, n := range StaticComponents() {
		out[n] = staticSpec{name: n}
	}
	return out
}
