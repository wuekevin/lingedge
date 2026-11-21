package installer

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// serverSpec 是 server 组件的生产 ComponentSpec（Story 1.35）。它没有测试替身成分：Start 经 systemctl 启动 lingedge-server.service，
// Health 对本机监听发真实 HTTP GET /healthz（setup mode 下该端点即存活探针，返回 200；/healthz/ready 要求数据库与迁移就绪，向导完成前必然 503，所以不用它）。
//
// server.yaml / master.key / setup-token.hash 由 installer 内建生成（serverconfig.go、preflight.go 的 renderConfigs），本规格的 InitConfig 不返回任何文件：
// 一来 server 的三件套有特殊语义（随机、已存在沿用、LoadCredential 降级），二来保证 Tools 不接触数据库口令、不写 migrate.yaml（AC6 ①）。
type serverSpec struct{}

// ServerExecutable 是 server 包内可执行文件（发行流水线的包布局：payload/bin/lingedge-server，scripts/release）。
const ServerExecutable = "payload/bin/lingedge-server"

func (serverSpec) Name() string { return "server" }

func (serverSpec) Executables(version, variant string) []string {
	return []string{ServerExecutable}
}

// ValidateSettings：server 的内建键（env、trusted-proxies）已由 installer 校验并过滤，到这里的都是未知键。
// 刻意不开放任何数据库/Redis/口令类的键：数据库与 Redis 的配置只能由安装向导完成（ADR-026），Tools 不接触口令。
func (serverSpec) ValidateSettings(settings map[string]string) error {
	if len(settings) == 0 {
		return nil
	}
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Errorf("server 不认识配置项 %s（可用：env、trusted-proxies、port.server.tcp.<端口>；数据库/Redis 由安装向导配置）", strings.Join(keys, "、"))
}

func (serverSpec) InitConfig(context.Context, ConfigInput) ([]ConfigFile, error) { return nil, nil }

func (serverSpec) Start(ctx context.Context, rt *Runtime) error { return rt.StartUnits(ctx) }

// Health 对 server 的 REST 监听发 GET /healthz。监听地址是预检算出的有效向导监听（rt.SetupListen，随 --set 换端口）；
// 通配/空主机映射为本机回环。2xx 才算健康。
func (serverSpec) Health(ctx context.Context, rt *Runtime) error {
	return HTTPOK(ctx, ServerHealthURL(rt.SetupListen), 3*time.Second)
}

// ServerHealthURL 由 server 的 REST 监听（host:port）得到本机 /healthz 的 URL；通配/空主机用回环，非法输入回落到缺省 127.0.0.1:8080。
func ServerHealthURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		host, port = "127.0.0.1", "8080"
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if a, perr := netip.ParseAddr(host); host == "" || (perr == nil && a.IsUnspecified()) {
		host = "127.0.0.1"
		if perr == nil && a.Is6() {
			host = "::1"
		}
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz"
}
