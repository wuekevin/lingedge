package installer

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 静态前端（admin-web / console / docs / status / website static）的 Nginx 站点片段渲染（Story 1.40，tools-design §11.3）。
//
// 渲染是纯函数：输入全部来自经校验的 Plan 值（域名、根目录、API 上游、TLS 文件），没有任何原始字符串被拼进配置——
// 每个值先过严格的字符集校验，再按规范形式输出（防配置注入）。生成的是一个可 include 的 server 块文件，**不写入任何既有 Nginx 配置**。

// apiMode 是片段对 /api/ 的处理方式。
type apiMode int

const (
	// apiNone：不反代 API；/api/ 一律 404（docs、status 是纯静态站点）。
	apiNone apiMode = iota
	// apiFull：/api/ 反代到 Server（Admin、Console：同源，会话 Cookie host-only）。
	apiFull
	// apiPublic：只放行 /api/v1/public/（官网公网域），/api/v1/public/setup 与其余 /api/ 返回 404（IMP-17）。
	apiPublic
)

// StaticProfile 描述一个静态前端的托管形态。
type StaticProfile struct {
	// SPA：历史路由（vue-router history 模式）→ 未命中静态文件时回落 /index.html；否则按静态多页处理，未命中 404。
	SPA bool
	API apiMode
	// Immutable 是内容哈希命名的资源路径前缀（Cache-Control: immutable）。
	Immutable []string
}

// staticProfiles 是已交付的静态前端；组件名与 resolver 的部署画像（profile.go 的 static 项）一致。
var staticProfiles = map[string]StaticProfile{
	"admin-web": {SPA: true, API: apiFull, Immutable: []string{"/assets/"}},
	"console":   {SPA: true, API: apiFull, Immutable: []string{"/assets/"}},
	"docs":      {API: apiNone},
	"status":    {API: apiNone},
	"website":   {API: apiPublic, Immutable: []string{"/_nuxt/"}},
}

// StaticComponents 返回静态前端组件名（字典序）。
func StaticComponents() []string {
	out := make([]string, 0, len(staticProfiles))
	for n := range staticProfiles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// IsStaticComponent 报告 name 是否静态前端组件。
func IsStaticComponent(name string) bool { _, ok := staticProfiles[name]; return ok }

// 约定路径（与 resolver 画像、tools-design §11.3 一致）。
const (
	nginxEtcDir = "/etc/lingedge/nginx"
	nginxConfD  = "/etc/nginx/conf.d"
)

// StaticConfPath 是组件的站点片段路径。
func StaticConfPath(c string) string { return nginxEtcDir + "/" + c + ".conf" }

// StaticHeadersPath 是组件的安全头文件路径。
func StaticHeadersPath(c string) string { return nginxEtcDir + "/" + c + "-security-headers.conf" }

// StaticRoot 是组件的站点根目录（current 链接 → releases/<v>/public）。
func StaticRoot(c string) string { return "/opt/lingedge/" + c + "/current/public" }

// StaticLinkPath 是原生 Nginx 的 conf.d 软链路径。
func StaticLinkPath(c string) string { return nginxConfD + "/lingedge-" + c + ".conf" }

// SiteParams 是渲染一个站点片段的全部输入。
type SiteParams struct {
	Component string
	Host      string
	// APIUpstream 规范形式 scheme://host[:port]；API 模式为 apiNone 时忽略。
	APIUpstream string
	// APICA 是 https 上游的 CA 文件（绝对路径）；http 上游为空。
	APICA     string
	TLSCert   string
	TLSKey    string
	HTTPPort  int
	HTTPSPort int
	IPv6      bool
}

// StaticSettings 是 --set 解析、校验后的结果（规范形式）。
type StaticSettings struct {
	Host        string
	APIUpstream string
	APICA       string
	TLSCert     string
	TLSKey      string
	HTTPPort    int
	HTTPSPort   int
	IPv6        bool
	// NginxLink：yes | no | ""（未指定：交互询问，非交互默认不创建）。
	NginxLink string
}

const defaultAPIUpstream = "http://127.0.0.1:8080"

var (
	reHostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reFilePath  = regexp.MustCompile(`^/[A-Za-z0-9_.+@/-]{1,255}$`)
)

// NormalizeHost 校验并规范化主机名（小写）：RFC 1123 标签，≤253 字节；拒绝通配符、IP 字面量、空白与任何非 ASCII（IDN 须先转 punycode）。
func NormalizeHost(v string) (string, error) {
	h := strings.ToLower(v)
	if h == "" || len(h) > 253 {
		return "", fmt.Errorf("host 必须是 1–253 字节的主机名")
	}
	if strings.ContainsAny(h, "*?_ \t/:@[]") || strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") {
		return "", fmt.Errorf("host 必须是不含通配符/端口/路径/结尾点的 DNS 主机名（如 admin.example.com）")
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return "", fmt.Errorf("host 不能是 IP 字面量：Admin、Console、官网需要互不相同的主机名（同一 IP 无法区分）")
	}
	allDigit := true
	for _, l := range strings.Split(h, ".") {
		if !reHostLabel.MatchString(l) {
			return "", fmt.Errorf("host 的标签 %q 不合法（小写字母、数字、连字符，首尾为字母数字，每段 ≤63）", l)
		}
		if strings.Trim(l, "0123456789") != "" {
			allDigit = false
		}
	}
	if allDigit {
		return "", fmt.Errorf("host 不能是纯数字（形似 IP 地址）")
	}
	return h, nil
}

// normalizeUpstream 把 api-upstream 规范化为 scheme://host[:port]：http|https，无用户信息/路径/查询，主机是 IP 或 DNS 名。
func normalizeUpstream(v string) (string, error) {
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("api-upstream 必须是 http(s)://主机[:端口]")
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("api-upstream 只能是 scheme://主机[:端口]，不得含用户信息、路径、查询或片段（/api/ 的路径由片段原样转发）")
	}
	host, port := u.Hostname(), u.Port()
	var hostOut string
	if a, err := netip.ParseAddr(host); err == nil {
		hostOut = a.String()
		if a.Is6() {
			hostOut = "[" + hostOut + "]"
		}
	} else {
		h := strings.ToLower(host)
		if h == "" || len(h) > 253 || strings.ContainsAny(h, "*?_ \t/:@[]") || strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") {
			return "", fmt.Errorf("api-upstream 的主机不合法")
		}
		for _, l := range strings.Split(h, ".") {
			if !reHostLabel.MatchString(l) {
				return "", fmt.Errorf("api-upstream 的主机标签 %q 不合法", l)
			}
		}
		hostOut = h
	}
	out := u.Scheme + "://" + hostOut
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return "", fmt.Errorf("api-upstream 的端口必须是 1–65535 的十进制整数")
		}
		out += ":" + port
	}
	return out, nil
}

func checkAbsPath(key, v string) error {
	if !reFilePath.MatchString(v) || strings.Contains(v, "//") {
		return fmt.Errorf("%s 必须是绝对路径（字符限 A-Za-z0-9_.+@/-，≤256）", key)
	}
	for _, seg := range strings.Split(v, "/") {
		if seg == ".." || seg == "." {
			return fmt.Errorf("%s 不得含 . 或 .. 路径段", key)
		}
	}
	return nil
}

func parsePortSetting(key, v string, def int) (int, error) {
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != v {
		return 0, fmt.Errorf("%s 必须是 1–65535 的十进制整数", key)
	}
	return n, nil
}

// ParseStaticSettings 校验并规范化 --set（非端口键）。host 必填；其余可选。错误只回显键名与规则，不回显取值。
func ParseStaticSettings(component string, settings map[string]string) (StaticSettings, error) {
	prof, ok := staticProfiles[component]
	if !ok {
		return StaticSettings{}, fmt.Errorf("%s 不是静态前端组件", component)
	}
	allowed := map[string]bool{"host": true, "tls-cert": true, "tls-key": true, "http-port": true, "https-port": true, "ipv6": true, "nginx-link": true}
	if prof.API != apiNone {
		allowed["api-upstream"], allowed["api-ca"] = true, true
	}
	var unknown []string
	for k := range settings {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		keys := make([]string, 0, len(allowed))
		for k := range allowed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return StaticSettings{}, fmt.Errorf("%s 不认识配置项 %s（可用：%s）", component, strings.Join(unknown, "、"), strings.Join(keys, "、"))
	}
	var s StaticSettings
	hv, has := settings["host"]
	if !has || hv == "" {
		return s, fmt.Errorf("缺少必填项 host：Admin、Console、官网必须使用互不相同的主机名，请用 --set host=<域名> 指定（如 --set host=admin.example.com）")
	}
	var err error
	if s.Host, err = NormalizeHost(hv); err != nil {
		return s, err
	}
	if prof.API != apiNone {
		s.APIUpstream = defaultAPIUpstream
		if v, ok := settings["api-upstream"]; ok {
			if s.APIUpstream, err = normalizeUpstream(v); err != nil {
				return s, err
			}
		}
		if v, ok := settings["api-ca"]; ok {
			if err := checkAbsPath("api-ca", v); err != nil {
				return s, err
			}
			s.APICA = v
		}
		https := strings.HasPrefix(s.APIUpstream, "https://")
		switch {
		case https && s.APICA == "":
			return s, fmt.Errorf("https 上游必须同时给 api-ca（CA 证书文件的绝对路径）：Tools 不假定各发行版的 CA 路径，也不会在不校验证书的情况下代理到 https 上游")
		case !https && s.APICA != "":
			return s, fmt.Errorf("api-ca 只用于 https 上游")
		}
	}
	cert, key := settings["tls-cert"], settings["tls-key"]
	if (cert == "") != (key == "") {
		return s, fmt.Errorf("tls-cert 与 tls-key 必须同时给出")
	}
	if cert != "" {
		if err := checkAbsPath("tls-cert", cert); err != nil {
			return s, err
		}
		if err := checkAbsPath("tls-key", key); err != nil {
			return s, err
		}
		s.TLSCert, s.TLSKey = cert, key
	}
	if s.HTTPPort, err = parsePortSetting("http-port", settings["http-port"], 80); err != nil {
		return s, err
	}
	if s.HTTPSPort, err = parsePortSetting("https-port", settings["https-port"], 443); err != nil {
		return s, err
	}
	if s.TLSCert != "" && s.HTTPPort == s.HTTPSPort {
		return s, fmt.Errorf("http-port 与 https-port 不能相同")
	}
	s.IPv6 = true
	switch settings["ipv6"] {
	case "", "yes":
	case "no":
		s.IPv6 = false
	default:
		return s, fmt.Errorf("ipv6 只能是 yes 或 no")
	}
	switch v := settings["nginx-link"]; v {
	case "", "yes", "no":
		s.NginxLink = v
	default:
		return s, fmt.Errorf("nginx-link 只能是 yes 或 no")
	}
	return s, nil
}

// contentSecurityPolicy 是静态前端的 CSP，与 Admin 的 Playwright 服务端（frontend/admin/e2e/serve.mjs；E2E 在该 CSP 下断言零违规）一致：
// 默认全部禁止，脚本只允许同源外链（构建产物无内联脚本）；样式允许内联（Arco 的组件以 style 属性设置尺寸/位置，去掉 'unsafe-inline' 会使组件错位）；
// 图片/字体允许同源与 data:；XHR 只允许同源（/api/ 由本片段同源反代）；frame/base 一律禁止。
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// RenderSecurityHeaders 渲染安全头集中文件（每条带 always）。hsts 仅在站点启用 TLS 时写入。
func RenderSecurityHeaders(component string, hsts bool) []byte {
	var b strings.Builder
	b.WriteString("# lingedge-security-headers: component=" + component + "\n")
	b.WriteString("# 安全头集中文件（tools-design §11.3）：每条带 always；站点片段在每个含 add_header 的 location 内显式 include 本文件\n")
	b.WriteString("# （nginx 的 add_header 不继承：location 内一旦有 add_header，上级的就全部失效）。\n")
	hdr := func(k, v string) { fmt.Fprintf(&b, "add_header %s %s always;\n", k, nginxQuote(v)) }
	hdr("X-Content-Type-Options", "nosniff")
	hdr("X-Frame-Options", "DENY")
	hdr("Referrer-Policy", "strict-origin-when-cross-origin")
	hdr("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	hdr("Cross-Origin-Opener-Policy", "same-origin")
	hdr("Content-Security-Policy", contentSecurityPolicy)
	if hsts {
		hdr("Strict-Transport-Security", "max-age=31536000")
	}
	return []byte(b.String())
}

// nginxQuote 输出双引号字符串；取值全部来自本文件的常量，这里仍拒绝引号与换行（防止将来误传入不受信值）。
func nginxQuote(v string) string {
	if strings.ContainsAny(v, "\"\\\n\r$") {
		panic("nginxQuote: 取值含不允许的字符")
	}
	return `"` + v + `"`
}

// SiteMarker 是片段首行的机器可读标记（独立主机名校验据此读取已安装的前端使用的主机名）。
const siteMarkerPrefix = "# lingedge-site: "

// ParseSiteMarker 从片段首部读取 component 与 host；没有标记返回 ok=false。
func ParseSiteMarker(conf []byte) (component, host string, ok bool) {
	line, _, _ := strings.Cut(string(conf), "\n")
	rest, found := strings.CutPrefix(line, siteMarkerPrefix)
	if !found {
		return "", "", false
	}
	for _, f := range strings.Fields(rest) {
		k, v, _ := strings.Cut(f, "=")
		switch k {
		case "component":
			component = v
		case "host":
			host = v
		}
	}
	return component, host, component != "" && host != ""
}

func (p SiteParams) validate() error {
	prof, ok := staticProfiles[p.Component]
	if !ok {
		return fmt.Errorf("%s 不是静态前端组件", p.Component)
	}
	if h, err := NormalizeHost(p.Host); err != nil || h != p.Host {
		return fmt.Errorf("host 不合法或未规范化")
	}
	if prof.API != apiNone {
		if u, err := normalizeUpstream(p.APIUpstream); err != nil || u != p.APIUpstream {
			return fmt.Errorf("api-upstream 不合法或未规范化")
		}
		if strings.HasPrefix(p.APIUpstream, "https://") != (p.APICA != "") {
			return fmt.Errorf("https 上游须有 api-ca，http 上游不得有")
		}
		if p.APICA != "" {
			if err := checkAbsPath("api-ca", p.APICA); err != nil {
				return err
			}
		}
	}
	if (p.TLSCert == "") != (p.TLSKey == "") {
		return fmt.Errorf("tls-cert 与 tls-key 必须同时给出")
	}
	for _, f := range []string{p.TLSCert, p.TLSKey} {
		if f != "" {
			if err := checkAbsPath("tls", f); err != nil {
				return err
			}
		}
	}
	if p.HTTPPort < 1 || p.HTTPPort > 65535 || p.HTTPSPort < 1 || p.HTTPSPort > 65535 {
		return fmt.Errorf("端口不合法")
	}
	return nil
}

// RenderStaticSite 渲染站点片段（server 块）与安全头文件。输出确定（同输入逐字节相同），且通过 LintNginxSnippet/LintSecurityHeaders
// （渲染器不得产出违规片段——不满足则返回错误而不是写出去）。
func RenderStaticSite(p SiteParams) (conf, headers []byte, err error) {
	if err := p.validate(); err != nil {
		return nil, nil, err
	}
	prof := staticProfiles[p.Component]
	hdrFile := StaticHeadersPath(p.Component)
	tls := p.TLSCert != ""
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }

	w("%scomponent=%s host=%s\n", siteMarkerPrefix, p.Component, p.Host)
	w("# 由 `lingedge install %s` 生成（tools-design §11.3）；只生成到本文件，不写入任何既有 Nginx 配置。\n", p.Component)
	w("# 使用：在 http{} 内 include 本文件（原生 Nginx：安装时可选自动创建 %s 软链并 nginx -t && reload）；\n", StaticLinkPath(p.Component))
	w("#       宝塔：面板里新建站点（域名 %s），include 本文件，或把站点 root 指向 %s。\n", p.Host, StaticRoot(p.Component))
	w("# 本文件由 Tools 管理：已存在且内容不同时安装不会覆盖，新内容写入 .new 供人工比对。\n\n")

	listen := func(port int, suffix string) {
		w("    listen %d%s;\n", port, suffix)
		if p.IPv6 {
			w("    listen [::]:%d%s;\n", port, suffix)
		}
	}
	if tls {
		target := "https://" + p.Host
		if p.HTTPSPort != 443 {
			target += ":" + strconv.Itoa(p.HTTPSPort)
		}
		w("server {\n")
		listen(p.HTTPPort, "")
		w("    server_name %s;\n", p.Host)
		w("    server_tokens off;\n\n")
		w("    location / {\n        return 301 %s$request_uri;\n    }\n", target)
		w("}\n\n")
	}

	w("server {\n")
	if tls {
		listen(p.HTTPSPort, " ssl http2")
	} else {
		listen(p.HTTPPort, "")
	}
	w("    server_name %s;\n", p.Host)
	w("    server_tokens off;\n")
	if tls {
		w("    ssl_certificate %s;\n    ssl_certificate_key %s;\n", p.TLSCert, p.TLSKey)
		w("    ssl_protocols TLSv1.3;\n")
	}
	w("\n    root %s;\n    index index.html;\n", StaticRoot(p.Component))
	w("    # 预压缩资源（发行包内随附 .gz）：需要 ngx_http_gzip_static_module（各发行版官方包均带；缺失时 nginx -t 会明确报错）。\n")
	w("    gzip_static on;\n    gzip_vary on;\n")

	proxy := func(loc string) {
		w("\n    location ^~ %s {\n", loc)
		w("        proxy_pass %s;\n", p.APIUpstream)
		w("        proxy_http_version 1.1;\n")
		w("        proxy_set_header Host $host;\n")
		w("        proxy_set_header X-Real-IP $remote_addr;\n")
		w("        proxy_set_header X-Forwarded-For $remote_addr;\n") // 覆盖而不追加：不信任访客自带的 X-Forwarded-For
		w("        proxy_set_header X-Forwarded-Proto $scheme;\n")
		if p.APICA != "" {
			w("        proxy_ssl_server_name on;\n        proxy_ssl_protocols TLSv1.2 TLSv1.3;\n        proxy_ssl_verify on;\n        proxy_ssl_trusted_certificate %s;\n", p.APICA)
		}
		w("        proxy_read_timeout 60s;\n    }\n")
	}
	deny := func(match, loc string) {
		w("\n    location %s%s {\n        return 404;\n    }\n", match, loc)
	}
	switch prof.API {
	case apiFull:
		proxy("/api/")
	case apiPublic:
		// /api/v1/public/setup/* 属于 /api/v1/public/ 前缀之内，必须显式堵上（安装向导接口不得经官网公网域可达，tools-design §11.3）。
		w("\n    # 官网公网域只放行 /api/v1/public/（其下的 setup 向导接口显式 404），其余 /api/ 一律 404：Admin/Console/认证接口在官网域不可达。\n")
		deny("= ", "/api/v1/public/setup")
		deny("^~ ", "/api/v1/public/setup/")
		proxy("/api/v1/public/")
		deny("^~ ", "/api/")
	default:
		deny("^~ ", "/api/")
	}

	w("\n    # 点文件不对外（/.well-known/ 除外）。\n")
	w("    location ~ /\\.(?!well-known/) {\n        return 404;\n    }\n")

	for _, imm := range prof.Immutable {
		w("\n    # 内容哈希命名的构建产物：永久缓存。\n    location ^~ %s {\n", imm)
		w("        include %s;\n", hdrFile)
		w("        add_header Cache-Control \"public, max-age=31536000, immutable\" always;\n")
		w("        try_files $uri =404;\n    }\n")
	}
	if prof.SPA {
		w("\n    # 入口页不缓存：发新版本后浏览器立即拿到新的哈希资源名。\n    location = /index.html {\n")
		w("        include %s;\n        add_header Cache-Control \"no-store\" always;\n    }\n", hdrFile)
		w("\n    # SPA 历史路由：未命中静态文件回落入口页（/api/ 已在上面单独处理，不会落到这里）。\n    location / {\n")
		w("        include %s;\n        add_header Cache-Control \"no-store\" always;\n        try_files $uri /index.html;\n    }\n", hdrFile)
	} else {
		w("\n    location / {\n        include %s;\n        add_header Cache-Control \"no-cache\" always;\n        try_files $uri $uri/index.html $uri.html =404;\n    }\n", hdrFile)
	}
	w("}\n")

	conf = []byte(b.String())
	headers = RenderSecurityHeaders(p.Component, tls)
	if bad := LintNginxSnippet(string(conf), hdrFile); len(bad) > 0 {
		return nil, nil, fmt.Errorf("渲染器产出了违规的 Nginx 片段（内部错误）：%s", strings.Join(bad, "；"))
	}
	if bad := LintSecurityHeaders(string(headers)); len(bad) > 0 {
		return nil, nil, fmt.Errorf("渲染器产出了违规的安全头文件（内部错误）：%s", strings.Join(bad, "；"))
	}
	return conf, headers, nil
}

// SiteURL 是站点的访问地址。
func (s StaticSettings) SiteURL() string {
	if s.TLSCert != "" {
		if s.HTTPSPort != 443 {
			return "https://" + net.JoinHostPort(s.Host, strconv.Itoa(s.HTTPSPort)) + "/"
		}
		return "https://" + s.Host + "/"
	}
	if s.HTTPPort != 80 {
		return "http://" + net.JoinHostPort(s.Host, strconv.Itoa(s.HTTPPort)) + "/"
	}
	return "http://" + s.Host + "/"
}
