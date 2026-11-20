package redisx

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/config"
)

// 缺省超时：Redis 慢于此即视为故障并降级，绝不让缓存/限流拖慢请求路径。
const (
	DefaultDialTimeout   = time.Second
	DefaultReadTimeout   = 500 * time.Millisecond
	DefaultWriteTimeout  = 500 * time.Millisecond
	DefaultProbeInterval = 2 * time.Second
	DefaultPoolSize      = 16
)

// Config 是 Redis 连接参数。Password 是 config.Secret：格式化/序列化只得到 ***。
type Config struct {
	Addr          string
	DB            int
	Env           string // 键空间的 <env>
	Password      config.Secret
	TLS           bool
	TLSCAFile     string
	TLSServerName string

	DialTimeout   time.Duration // 0 → DefaultDialTimeout
	ReadTimeout   time.Duration
	WriteTimeout  time.Duration
	ProbeInterval time.Duration // 降级/健康探测间隔
	PoolSize      int
}

// FromConfig 由平台配置构造（Env 取 server.env）。
func FromConfig(c config.Config) Config {
	return Config{
		Addr: c.Redis.Addr, DB: c.Redis.DB, Env: c.Server.Env, Password: c.Redis.Password,
		TLS: c.Redis.TLS, TLSCAFile: c.Redis.TLSCAFile, TLSServerName: c.Redis.TLSServerName,
	}
}

// String 实现 fmt.Stringer：不含口令（口令由 Secret 掩码，其余字段无敏感信息）。
func (c Config) String() string {
	return fmt.Sprintf("redisx.Config{addr=%s db=%d env=%s password=%s tls=%t}", c.Addr, c.DB, c.Env, c.Password, c.TLS)
}

func (c Config) withDefaults() Config {
	if c.DialTimeout <= 0 {
		c.DialTimeout = DefaultDialTimeout
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = DefaultReadTimeout
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = DefaultWriteTimeout
	}
	if c.ProbeInterval <= 0 {
		c.ProbeInterval = DefaultProbeInterval
	}
	if c.PoolSize <= 0 {
		c.PoolSize = DefaultPoolSize
	}
	return c
}

// validate 校验配置错误（这类错误是部署错误，New 直接失败；与「Redis 不可达」不同）。
func (c Config) validate() error {
	if _, err := NewKeyspace(c.Env); err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(c.Addr)
	if err != nil || host == "" || port == "" {
		return errors.New("redisx: addr 必须是 <主机>:<端口>")
	}
	if c.DB < 0 || c.DB > 15 {
		return errors.New("redisx: db 必须在 0–15")
	}
	if !c.TLS && (c.TLSCAFile != "" || c.TLSServerName != "") {
		return errors.New("redisx: 设置了 tls_ca_file/tls_server_name 时必须启用 tls")
	}
	return nil
}

func (c Config) tlsConfig() (*tls.Config, error) {
	if !c.TLS {
		return nil, nil
	}
	host, _, _ := net.SplitHostPort(c.Addr)
	tc := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host} // 证书校验不可关闭：没有 InsecureSkipVerify 入口
	if c.TLSServerName != "" {
		tc.ServerName = c.TLSServerName
	}
	if c.TLSCAFile != "" {
		pem, err := os.ReadFile(c.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("redisx: 读取 tls_ca_file 失败: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("redisx: tls_ca_file 不含有效的 PEM 证书")
		}
		tc.RootCAs = pool
	}
	return tc, nil
}
