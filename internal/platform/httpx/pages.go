package httpx

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
)

// HandlePage 注册一个非 REST 的静态页面（GET，只允许 /setup 及其子路径：安装向导的内嵌页面，Story 1.24）。
// 它不属于 openapi（不进入路由清单、不带路由元数据）：页面本身不含任何数据与口令，只是向导前端的载体；
// 向导的所有数据操作都经 /api/v1/public/setup/*（有完整元数据、token 与审计）。任何其他路径一律拒绝，防止借此绕开 Register 的元数据校验。
func (r *Router) HandlePage(path string, h gin.HandlerFunc) (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer func() {
		if err != nil {
			r.errs = append(r.errs, err)
		}
	}()
	if h == nil {
		return fmt.Errorf("httpx: 页面 %q 的 handler 为空", path)
	}
	if path != "/setup" && !strings.HasPrefix(path, "/setup/") {
		return fmt.Errorf("httpx: 页面路径只允许 /setup 及其子路径，实际 %q", path)
	}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("httpx: 注册页面 %s 失败: %v", path, rec)
			}
		}()
		r.engine.GET(path, h)
	}()
	return err
}

// SetFallback 替换「未匹配任何路由」时的响应（缺省 404 resource.not_found）。setup mode 用它把所有未注册的路径统一为 503（Story 1.24 AC1）：
// 此时除 /healthz、/setup 与 /api/v1/public/setup/* 外没有任何可用接口。
func (r *Router) SetFallback(h gin.HandlerFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.engine.NoRoute(h)
	r.engine.NoMethod(h)
}
