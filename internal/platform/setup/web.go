package setup

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

// webdist 是安装向导 SPA（frontend/setup，Vue 3 + TS）的生产构建产物（Story 1.37、tools-design §13「go:embed 的轻量 SPA，不依赖 Admin Web」）。
//
// 产物**入库**，由 `make setup-web` 生成并同步；`make verify-build`（经 frontend 目标）重新构建并逐字节比对（漂移 = 失败，缺失 = 失败），
// 因此 `go build`/`go test` 不依赖 Node，而嵌入内容仍然可重现、不可悄悄过期。选择理由见 frontend/README「安装向导的嵌入」。
// 此处故意用 all: 前缀：产物里 `_` 或 `.` 开头的文件（如将来的 _plugin-vue_export-helper）也必须被嵌入。
//
//go:embed all:webdist
var webEmbed embed.FS

const (
	pagePath   = "/setup"
	assetsPath = "/setup/assets/"
	// indexName 是 SPA 入口；其引用的资源都在 assets/ 下（vite base=/setup/）。
	indexName = "index.html"
	// 向导页的 CSP：只放行同源脚本/样式/连接，无内联脚本、无 eval、无外部资源（脚本样式都是 self；Vue 经 CSSOM 设置样式不受 style-src 限制）。
	webCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
)

var (
	webOnce sync.Once
	webRoot fs.FS
	webErr  error
)

// webFiles 返回 webdist 子树并校验其完整性（入口存在、入口引用的每个资源都在）：缺失 = 构建缺陷，setup mode 直接启动失败（fail-closed），
// 而不是给操作者一个空白页。
func webFiles() (fs.FS, error) {
	webOnce.Do(func() {
		sub, err := fs.Sub(webEmbed, "webdist")
		if err != nil {
			webErr = err
			return
		}
		webErr = verifyWeb(sub)
		if webErr == nil {
			webRoot = sub
		}
	})
	return webRoot, webErr
}

var reAssetRef = regexp.MustCompile(`(?:src|href)="(/setup/assets/[^"]+)"`)

// verifyWeb 检查产物完整：index.html 存在且非空、至少引用一个脚本、所有被引用的资源存在；入口不得含内联脚本。
func verifyWeb(root fs.FS) error {
	index, err := fs.ReadFile(root, indexName)
	if err != nil || len(bytes.TrimSpace(index)) == 0 {
		return errors.New("setup: 内嵌的向导前端缺少 index.html（构建缺陷：先运行 make setup-web 并提交 internal/platform/setup/webdist）")
	}
	refs := reAssetRef.FindAllSubmatch(index, -1)
	if len(refs) == 0 {
		return errors.New("setup: 内嵌的向导前端 index.html 没有引用任何 /setup/assets/ 资源")
	}
	hasScript := false
	for _, m := range refs {
		ref := string(m[1])
		rel := strings.TrimPrefix(ref, "/setup/")
		if b, err := fs.ReadFile(root, rel); err != nil || len(b) == 0 {
			return fmt.Errorf("setup: 内嵌的向导前端缺少被引用的资源 %s", ref)
		}
		if strings.HasSuffix(ref, ".js") {
			hasScript = true
		}
	}
	if !hasScript {
		return errors.New("setup: 内嵌的向导前端没有脚本资源")
	}
	if bytes.Contains(bytes.ToLower(index), []byte("<script>")) || bytes.Contains(bytes.ToLower(index), []byte("javascript:")) {
		return errors.New("setup: 内嵌的向导入口不得含内联脚本（CSP script-src 'self'）")
	}
	return nil
}

func contentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json; charset=utf-8"
	case ".woff2":
		return "font/woff2"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	}
	return "application/octet-stream"
}

func securityHeaders(c *gin.Context) {
	c.Header("Content-Security-Policy", webCSP)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Frame-Options", "DENY")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("Cross-Origin-Opener-Policy", "same-origin")
}

// RegisterPage 注册内嵌的向导 SPA（仅 setup mode）：GET /setup（入口，no-store）与 GET /setup/assets/*（带内容哈希的静态资源）。
// 内嵌产物不完整时返回错误，使 setup mode 启动失败（而不是提供空白页）。
func (s *Service) RegisterPage(r *httpx.Router) error {
	root, err := webFiles()
	if err != nil {
		return err
	}
	serveIndex := func(c *gin.Context) {
		b, err := fs.ReadFile(root, indexName)
		if err != nil {
			httpx.Fail(c, apierr.Internal(err))
			return
		}
		c.Header("Cache-Control", "no-store")
		securityHeaders(c)
		c.Data(http.StatusOK, contentType(indexName), b)
	}
	serveAsset := func(c *gin.Context) {
		name := path.Clean("assets/" + strings.TrimPrefix(c.Param("filepath"), "/"))
		if !strings.HasPrefix(name, "assets/") || strings.Contains(name, "..") {
			httpx.Fail(c, apierr.NotFound())
			return
		}
		b, err := fs.ReadFile(root, name)
		if err != nil {
			httpx.Fail(c, apierr.NotFound())
			return
		}
		// 文件名含内容哈希：可长期缓存；入口页 no-store，故新版本总能取到新的资源名。
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
		securityHeaders(c)
		c.Data(http.StatusOK, contentType(name), b)
	}
	return errors.Join(
		r.HandlePage(pagePath, serveIndex),
		r.HandlePage(pagePath+"/", serveIndex),
		r.HandlePage(assetsPath+"*filepath", serveAsset),
	)
}

// RegisterGonePages 在正常模式（已安装，install.lock 存在）下注册 /setup 与 /setup/* 的 410：与 /api/v1/public/setup/* 一致返回
// `410 setup.already_installed`（MIN-04、tools-design §13；不是 404：已知曾存在向导，向运营者明确说明它已永久关闭）。
func (s *Service) RegisterGonePages(r *httpx.Router) error {
	gone := func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		httpx.Fail(c, apierr.New(apierr.SetupAlreadyInstalled, "setup already completed"))
	}
	return errors.Join(
		r.HandlePage(pagePath, gone),
		r.HandlePage(pagePath+"/", gone),
		r.HandlePage(assetsPath+"*filepath", gone),
	)
}
