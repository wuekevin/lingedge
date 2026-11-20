package httpx

import "context"

type clientIPKey struct{}

// ClientIPFrom 取出本请求的来源 IP：由路由器的 Gin 引擎按 Options.TrustedProxies 解析（只有来自可信反代的连接才采信 X-Forwarded-For，
// 取其中最右侧的非可信地址）——与访问日志、登录日志、节流使用的是同一个取值（c.ClientIP()）。
// 认证钩子拿到的是 *http.Request（没有 gin.Context），需要来源 IP 的钩子（API Token 的 allowed_cidrs，Story 1.19）必须经它读取，
// 不得自行解析 X-Forwarded-For（否则伪造该头即可绕过来源限制）。没有登记（不经路由器）时返回空串，调用方应按「来源未知」fail-closed。
func ClientIPFrom(ctx context.Context) string {
	s, _ := ctx.Value(clientIPKey{}).(string)
	return s
}

func withClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}
