package probe

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/security"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
)

// Endpoint 是一个待探测的 TCP 端点。
type Endpoint struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func (e Endpoint) addr() string { return net.JoinHostPort(e.Host, strconv.Itoa(e.Port)) }

func (e Endpoint) loopback() bool {
	if e.Host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(e.Host)
	return err == nil && ip.Unmap().IsLoopback()
}

// Creds 是调用方提供的凭据（来自 Server 向导）。它只存在于这个结构里：不写入 Result、不进日志，错误文本经脱敏并显式剔除口令。
// 零值表示不提供凭据——此时只做免凭据探测。
type Creds struct {
	MySQLUser     string
	MySQLPassword string
	RedisUsername string // Redis 6+ ACL 用户；空 = 只用口令的 AUTH
	RedisPassword string
}

// DepStatus 是一个依赖端点的探测结果（不含任何凭据）。
type DepStatus struct {
	Kind          string   `json:"kind"` // mysql | redis
	Endpoint      Endpoint `json:"endpoint"`
	Reachable     bool     `json:"reachable"`
	Version       string   `json:"version,omitempty"`
	Flavor        string   `json:"flavor,omitempty"` // mysql | mariadb | redis
	AuthRequired  bool     `json:"auth_required,omitempty"`
	Authenticated bool     `json:"authenticated,omitempty"` // 提供了凭据且认证成功
	Error         string   `json:"error,omitempty"`         // 已脱敏
}

const defaultDialTimeout = 3 * time.Second

// ProbeMySQL 探测 MySQL/MariaDB 端点。
//
// 免凭据：TCP 连接后只读取服务器主动发来的握手包（协议版本 + 服务器版本串），不发送任何认证数据，随即关闭——
// 版本与连通性无需口令。注意：握手后不认证就断开会被 MySQL 计入该主机的 `Aborted_connects`/`max_connect_errors`
// （默认 100），所以每次 Collect 对每个端点只探测一次。
//
// 有凭据（MySQLUser 非空）：经 safeexec 执行 `mysql` 客户端，只执行 `SELECT VERSION()`（只读）。口令经环境变量 MYSQL_PWD 传入——
// 不进 argv（ps 看不到），不落盘；客户端缺失时认证探测不可用，仍给出免凭据结果并在 Error 中说明。
func ProbeMySQL(ctx context.Context, r *safeexec.Runner, ep Endpoint, c Creds, timeout time.Duration) DepStatus {
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	st := DepStatus{Kind: "mysql", Endpoint: ep}
	if c.MySQLUser != "" {
		v, err := mysqlSelectVersion(ctx, r, ep, c, timeout)
		if err == nil {
			st.Reachable, st.Authenticated, st.Version = true, true, v
			st.Flavor = flavorOfMySQL(v)
			return st
		}
		st.Error = sanitize(err, c.MySQLPassword)
		var ee *safeexec.ExitError
		if errors.As(err, &ee) { // 客户端已运行并连上，被服务器拒绝（或语句失败）：需要有效凭据
			st.AuthRequired = true
		}
	}
	ver, err := mysqlGreeting(ctx, ep, timeout)
	if err != nil {
		if !st.AuthRequired {
			st.Error = sanitize(err, c.MySQLPassword)
		}
		return st
	}
	st.Reachable, st.Version = true, ver
	st.Flavor = flavorOfMySQL(ver)
	if c.MySQLUser == "" {
		st.Error = ""
	}
	return st
}

func flavorOfMySQL(v string) string {
	if strings.Contains(strings.ToLower(v), "mariadb") {
		return "mariadb"
	}
	return "mysql"
}

// mysqlGreeting 读取服务器握手包（Initial Handshake Packet）：3 字节长度 + 1 字节序号 + 载荷（协议版本 0x0a + 以 NUL 结尾的服务器版本串…）。
func mysqlGreeting(ctx context.Context, ep Endpoint, timeout time.Duration) (string, error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", ep.addr())
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	return ParseMySQLGreeting(bufio.NewReader(io.LimitReader(conn, 4096)))
}

// ParseMySQLGreeting 解析握手包，返回服务器版本串。MariaDB 为兼容旧客户端会在版本前加 `5.5.5-` 前缀，这里去掉。
func ParseMySQLGreeting(r io.Reader) (string, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", fmt.Errorf("读取握手包失败: %w", err)
	}
	n := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	if n < 2 || n > 4096 {
		return "", errors.New("握手包长度异常（不是 MySQL 协议？）")
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return "", fmt.Errorf("读取握手包失败: %w", err)
	}
	switch payload[0] {
	case 0x0a:
	case 0xff:
		return "", errors.New("服务器返回错误包（可能拒绝该主机的连接）")
	default:
		return "", errors.New("握手包协议版本不是 10（不是 MySQL 协议？）")
	}
	end := 1
	for end < len(payload) && payload[end] != 0 {
		end++
	}
	if end == len(payload) || end == 1 {
		return "", errors.New("握手包缺少服务器版本串")
	}
	v := string(payload[1:end])
	v = strings.TrimPrefix(v, "5.5.5-")
	if !regexp.MustCompile(`^\d+\.\d+`).MatchString(v) {
		return "", errors.New("握手包的版本串格式异常")
	}
	return v, nil
}

var (
	reMyUser = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,64}$`)
	reMyHost = regexp.MustCompile(`^[A-Za-z0-9.:_-]{1,253}$`)
)

// mysqlSelectSQL 是认证探测唯一执行的语句。
const mysqlSelectSQL = "SELECT VERSION()"

// mysqlArgs 构造客户端的 argv（参数数组，不经 shell）。主机/用户名经严格字符集校验：不含空白与 shell 元字符，也不会以 - 开头被当成选项。
func mysqlArgs(ep Endpoint, user string, timeout time.Duration) ([]string, error) {
	if !reMyUser.MatchString(user) || strings.HasPrefix(user, "-") {
		return nil, errors.New("probe: MySQL 用户名含不允许的字符")
	}
	if !reMyHost.MatchString(ep.Host) || strings.HasPrefix(ep.Host, "-") || ep.Port < 1 || ep.Port > 65535 {
		return nil, errors.New("probe: MySQL 地址不合法")
	}
	secs := int(timeout / time.Second)
	if secs < 1 {
		secs = 1
	}
	return []string{"--no-defaults", "--protocol=tcp", "-h", ep.Host, "-P", strconv.Itoa(ep.Port), "-u", user,
		"--connect-timeout=" + strconv.Itoa(secs), "-N", "-B", "-e", mysqlSelectSQL}, nil
}

// mysqlArgvAllowed 校验一个 mysql 客户端 argv 恰好是 mysqlArgs 的形状（供只读 argv 表使用）。
func mysqlArgvAllowed(args []string) bool {
	if len(args) != 13 || args[0] != "--no-defaults" || args[1] != "--protocol=tcp" || args[2] != "-h" || args[4] != "-P" || args[6] != "-u" ||
		!strings.HasPrefix(args[8], "--connect-timeout=") || args[9] != "-N" || args[10] != "-B" || args[11] != "-e" || args[12] != mysqlSelectSQL {
		return false
	}
	port, err := strconv.Atoi(args[5])
	return err == nil && port >= 1 && port <= 65535 && reMyHost.MatchString(args[3]) && !strings.HasPrefix(args[3], "-") &&
		reMyUser.MatchString(args[7]) && !strings.HasPrefix(args[7], "-")
}

func mysqlSelectVersion(ctx context.Context, r *safeexec.Runner, ep Endpoint, c Creds, timeout time.Duration) (string, error) {
	args, err := mysqlArgs(ep, c.MySQLUser, timeout)
	if err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, timeout+2*time.Second)
	defer cancel()
	so, _, err := readOnlyRun(cctx, r, "mysql", args, "MYSQL_PWD="+c.MySQLPassword)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(so))
	if !regexp.MustCompile(`^\d+\.\d+`).MatchString(v) {
		return "", errors.New("probe: mysql 客户端的输出不是版本号")
	}
	return v, nil
}

// sanitize 生成可写入日志/报告的错误文本：先脱敏，再显式剔除口令字面量（驱动的错误文本理论上不含口令，这是纵深防御）。
func sanitize(err error, secrets ...string) string {
	if err == nil {
		return ""
	}
	s := security.RedactString(err.Error())
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, security.Mask)
		}
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// redisAllowed 是只读命令表：免凭据只有 PING；有凭据时增加 AUTH 与 INFO server。其他命令在发送前即被拒绝。
func redisAllowed(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch strings.ToUpper(args[0]) {
	case "PING":
		return len(args) == 1
	case "AUTH":
		return len(args) == 2 || len(args) == 3
	case "INFO":
		return len(args) == 2 && strings.EqualFold(args[1], "server")
	}
	return false
}

func respEncode(args []string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	return []byte(b.String())
}

// respReply 是一条 RESP2 回复：kind 为 + - $ : 之一。
type respReply struct {
	kind byte
	text string
}

const maxRedisBulk = 1 << 20

func respRead(r *bufio.Reader) (respReply, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return respReply{}, err
	}
	if len(line) < 3 || !strings.HasSuffix(line, "\r\n") {
		return respReply{}, errors.New("RESP 回复格式异常")
	}
	body := line[1 : len(line)-2]
	switch line[0] {
	case '+', '-', ':':
		return respReply{kind: line[0], text: body}, nil
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil || n < -1 || n > maxRedisBulk {
			return respReply{}, errors.New("RESP bulk 长度异常")
		}
		if n == -1 {
			return respReply{kind: '$'}, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return respReply{}, err
		}
		return respReply{kind: '$', text: string(buf[:n])}, nil
	}
	return respReply{}, errors.New("不支持的 RESP 回复类型（不是 Redis？）")
}

// ProbeRedis 探测 Redis 端点：免凭据只发 PING（+PONG = 可达且无需认证；-NOAUTH = 可达但需要口令）；
// 有口令时 AUTH 后发 `INFO server` 取 redis_version（只读）。口令只会发往环回地址——明文 AUTH 发往他机等于泄露，拒绝并报错。
func ProbeRedis(ctx context.Context, ep Endpoint, c Creds, timeout time.Duration) DepStatus {
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	st := DepStatus{Kind: "redis", Endpoint: ep}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", ep.addr())
	if err != nil {
		st.Error = sanitize(err, c.RedisPassword)
		return st
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	r := bufio.NewReader(io.LimitReader(conn, 4<<20))
	send := func(args ...string) (respReply, error) {
		if !redisAllowed(args) {
			return respReply{}, fmt.Errorf("probe: Redis 命令 %q 不在只读白名单内", args[0])
		}
		if _, err := conn.Write(respEncode(args)); err != nil {
			return respReply{}, err
		}
		return respRead(r)
	}
	rep, err := send("PING")
	if err != nil {
		st.Error = sanitize(err, c.RedisPassword)
		return st
	}
	switch {
	case rep.kind == '+':
		st.Reachable = true
	case rep.kind == '-' && strings.HasPrefix(rep.text, "NOAUTH"):
		st.Reachable, st.AuthRequired = true, true
	case rep.kind == '-' && strings.HasPrefix(rep.text, "DENIED"): // protected-mode
		st.Reachable, st.AuthRequired = true, true
		st.Error = "Redis 处于 protected-mode，拒绝该来源连接"
		return st
	default:
		st.Error = "对 PING 的回复不是 Redis 协议"
		return st
	}
	if st.AuthRequired { // 无需认证的服务器不发 AUTH（否则会收到 "called without any password configured" 错误）
		if c.RedisPassword == "" {
			return st // 需要口令而调用方未提供：到此为止
		}
		if !ep.loopback() {
			st.Error = "拒绝向非环回地址明文发送 Redis 口令"
			return st
		}
		args := []string{"AUTH", c.RedisPassword}
		if c.RedisUsername != "" {
			args = []string{"AUTH", c.RedisUsername, c.RedisPassword}
		}
		ar, err := send(args...)
		if err != nil {
			st.Error = sanitize(err, c.RedisPassword)
			return st
		}
		if ar.kind == '-' {
			st.Error = sanitize(errors.New("AUTH 被拒绝: "+ar.text), c.RedisPassword)
			return st
		}
		st.Authenticated = true
	}
	// 无口令的 Redis 与已认证的连接都可以取 INFO server（只读）
	ir, err := send("INFO", "server")
	if err == nil && ir.kind == '$' {
		st.Version = ParseRedisInfoVersion(ir.text)
	}
	if st.Version != "" {
		st.Flavor = "redis"
		if f := redisFlavor(ir.text); f != "" {
			st.Flavor = f
		}
	}
	return st
}

// ParseRedisInfoVersion 解析 `INFO server`（或 redis-cli INFO）输出中的 redis_version。
func ParseRedisInfoVersion(info string) string {
	if m := reRedisI.FindStringSubmatch(info); m != nil {
		return m[1]
	}
	return ""
}

var reRedisServerName = regexp.MustCompile(`(?m)^server_name:(\S+)`)

// redisFlavor：Redis 7.4+/8 的 INFO 带 server_name（redis | valkey）。
func redisFlavor(info string) string {
	if m := reRedisServerName.FindStringSubmatch(info); m != nil && (m[1] == "valkey" || m[1] == "redis") {
		return m[1]
	}
	return ""
}

func sanitizeText(s string) string { return security.RedactString(s) }
