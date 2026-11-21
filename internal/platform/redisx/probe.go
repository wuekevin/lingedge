package redisx

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/wuekevin/lingedge/pkg/redact"
)

// MinRedisMajor 是最低 Redis 主版本（ADR-025；安装向导 Redis 步骤，tools-design §13）。
const MinRedisMajor = 8

// ServerInfo 是 `INFO server` 的关键事实。
type ServerInfo struct {
	Version string // redis_version，如 8.0.2
	Major   int
}

// ErrRedisVersion：Redis 主版本低于最低要求。
var ErrRedisVersion = errors.New("redisx: Redis 主版本低于最低要求")

// ParseInfoServer 解析 `INFO server` 的原始文本，取出 redis_version 与主版本（纯函数，便于对构造的响应做单元测试）。
func ParseInfoServer(info string) (ServerInfo, error) {
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		v, ok := strings.CutPrefix(line, "redis_version:")
		if !ok {
			continue
		}
		major, _, _ := strings.Cut(v, ".")
		n, err := strconv.Atoi(major)
		if err != nil || n < 0 {
			return ServerInfo{}, fmt.Errorf("redisx: 无法解析 redis_version %q", v)
		}
		return ServerInfo{Version: v, Major: n}, nil
	}
	return ServerInfo{}, errors.New("redisx: INFO server 响应里没有 redis_version")
}

// CheckMajor 校验主版本 ≥ MinRedisMajor。
func (s ServerInfo) CheckMajor() error {
	if s.Major < MinRedisMajor {
		return fmt.Errorf("%w：当前 %s，需要 ≥ %d（ADR-025）", ErrRedisVersion, s.Version, MinRedisMajor)
	}
	return nil
}

// Probe 以一个一次性连接对 Redis 做 `INFO server`（安装向导 Redis 步骤）：只读命令，绝不使用 FLUSHDB/FLUSHALL；
// 用完立即关闭；错误文本经脱敏，不含口令。Config.Env 可为空（此处不使用键空间）。
func Probe(ctx context.Context, cfg Config) (ServerInfo, error) {
	cfg = cfg.withDefaults()
	if cfg.Addr == "" {
		return ServerInfo{}, errors.New("redisx: addr 必须是 <主机>:<端口>")
	}
	if cfg.DB < 0 || cfg.DB > 15 {
		return ServerInfo{}, errors.New("redisx: db 必须在 0–15")
	}
	tc, err := cfg.tlsConfig()
	if err != nil {
		return ServerInfo{}, err
	}
	rdb := redis.NewClient(&redis.Options{Addr: cfg.Addr, DB: cfg.DB, Password: cfg.Password.Reveal(), TLSConfig: tc,
		DialTimeout: 3 * cfg.DialTimeout, ReadTimeout: 4 * cfg.ReadTimeout, WriteTimeout: 4 * cfg.WriteTimeout, PoolSize: 1, MaxRetries: -1, DisableIdentity: true})
	defer func() { _ = rdb.Close() }()
	raw, err := rdb.Info(ctx, "server").Result()
	if err != nil {
		msg := redact.String(err.Error())
		if pw := cfg.Password.Reveal(); pw != "" {
			msg = strings.ReplaceAll(msg, pw, "***")
		}
		return ServerInfo{}, fmt.Errorf("redisx: 无法连接 Redis 或 INFO 失败: %s", msg)
	}
	return ParseInfoServer(raw)
}
