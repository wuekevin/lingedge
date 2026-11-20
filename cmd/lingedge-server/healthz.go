package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// healthzMain 实现 `lingedge-server healthz`：对本机 Server 发 GET /healthz（--ready 时为 /healthz/ready），HTTP 200 → 退出码 0，
// 其余（含连接失败、超时、503）→ 1。供 Tools 安装后的健康检查与 systemd/监控脚本使用。
// 目标地址：--addr；否则取配置的 http.listen（通配地址换成 127.0.0.1）。就绪检查返回 200 且 status=degraded（Redis 降级）同样视为成功。
func healthzMain(args []string, stdout, stderr io.Writer, environ []string) int {
	fl := flag.NewFlagSet("healthz", flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfgPath := fl.String("config", "", "server.yaml 路径（取 http.listen；缺省 $"+EnvConfig+"，再缺省 "+DefaultConfigPath+"）")
	addr := fl.String("addr", "", "直接指定 host:port（给出时不读配置）")
	ready := fl.Bool("ready", false, "探测 /healthz/ready（含数据库/迁移/Redis）而不是存活 /healthz")
	timeout := fl.Duration("timeout", 3*time.Second, "请求超时")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() > 0 || *timeout <= 0 {
		_, _ = fmt.Fprintf(stderr, "healthz: 参数错误\n")
		return 2
	}
	target := *addr
	if target == "" {
		cfg, err := loadConfig(*cfgPath, environ, false)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "healthz: %v\n", err)
			return 1
		}
		target = cfg.HTTP.Listen
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "healthz: 地址 %q 不是 host:port\n", target)
		return 2
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	path := "/healthz"
	if *ready {
		path = "/healthz/ready"
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+path, nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "healthz: %v\n", err)
		return 1
	}
	resp, err := (&http.Client{Timeout: *timeout}).Do(req)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "healthz: 无法连接 %s%s: %v\n", net.JoinHostPort(host, port), path, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_, _ = fmt.Fprintf(stdout, "%s\n", body)
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(stderr, "healthz: HTTP %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
