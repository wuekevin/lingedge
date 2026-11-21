package downloader

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/security"
)

// 默认值（[ASSUMPTION]，UD-27：工程取值，非产品承诺，已在 decisions-adopted.md §1.1 登记）。
const (
	defaultConnectTimeout = 10 * time.Second
	defaultTLSTimeout     = 10 * time.Second
	defaultHeaderTimeout  = 30 * time.Second
	defaultIdleTimeout    = 30 * time.Second
	defaultAttemptTimeout = 30 * time.Minute
	defaultTotalTimeout   = 2 * time.Hour
	defaultMaxAttempts    = 4
	defaultBackoffBase    = time.Second
	defaultBackoffMax     = 8 * time.Second
	defaultMaxRedirects   = 5
	maxCAFileBytes        = 1 << 20
	userAgent             = "lingedge-tools/downloader"
)

// Config 是下载器的传输配置。零值字段取默认。没有任何关闭证书校验的开关（无 --insecure）。
type Config struct {
	// CAFile 是 release.ca_file：在系统根证书之外追加的 PEM 根证书（企业镜像/私有 CA）。空 = 只用系统根。
	CAFile string
	// ProxyURL 显式代理（http/https/socks5）；空 = 遵循环境变量 HTTPS_PROXY/NO_PROXY。
	ProxyURL string
	// ResumeDir 非空时启用「跨调用断点续传」：绝对路径，只属于当前用户（0700，不在 staging 内）。见 resume.go 的信任模型。
	ResumeDir string
	// ResumeTTL 是缓存条目的保留期（缺省 7 天），过期条目在下一次使用缓存时被清理。
	ResumeTTL time.Duration

	ConnectTimeout time.Duration // 建立 TCP 连接
	TLSTimeout     time.Duration // TLS 握手
	HeaderTimeout  time.Duration // 请求发出后等待响应头
	IdleTimeout    time.Duration // 响应体连续无数据的最长时间（慢速攻击/卡死）
	AttemptTimeout time.Duration // 单次尝试的总时长
	TotalTimeout   time.Duration // 含重试的总时长
	MaxAttempts    int           // 含首次的最大尝试次数
	BackoffBase    time.Duration // 重试退避：base、2*base…，不超过 BackoffMax
	BackoffMax     time.Duration
	MaxRedirects   int

	// OnRetry 在每次决定重试前调用（观测/测试用）。
	OnRetry func(attempt int, err error)
}

func (c Config) withDefaults() Config {
	d := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	d(&c.ConnectTimeout, defaultConnectTimeout)
	d(&c.TLSTimeout, defaultTLSTimeout)
	d(&c.HeaderTimeout, defaultHeaderTimeout)
	d(&c.IdleTimeout, defaultIdleTimeout)
	d(&c.AttemptTimeout, defaultAttemptTimeout)
	d(&c.TotalTimeout, defaultTotalTimeout)
	d(&c.BackoffBase, defaultBackoffBase)
	d(&c.BackoffMax, defaultBackoffMax)
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = defaultMaxAttempts
	}
	if c.MaxRedirects <= 0 {
		c.MaxRedirects = defaultMaxRedirects
	}
	return c
}

// Client 是只走 HTTPS 的下载客户端。
type Client struct {
	cfg Config
	hc  *http.Client
	tr  *http.Transport
}

// New 构造客户端：校验 CAFile/ProxyURL，装配 TLS（最低 TLS 1.2，系统根 + CAFile）与重定向策略。
func New(cfg Config) (*Client, error) {
	cfg = cfg.withDefaults()
	pool, err := rootPool(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	if cfg.ResumeDir != "" {
		if err := checkResumeDir(cfg.ResumeDir); err != nil {
			return nil, err
		}
	}
	proxy := http.ProxyFromEnvironment
	if cfg.ProxyURL != "" {
		pu, err := url.Parse(cfg.ProxyURL)
		if err != nil || pu.Host == "" {
			return nil, newErr(KindOptions, "代理地址非法")
		}
		switch pu.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, newErr(KindOptions, "代理协议 %q 不被支持（http|https|socks5）", pu.Scheme)
		}
		proxy = http.ProxyURL(pu)
	}
	tr := &http.Transport{
		Proxy:                 proxy,
		DialContext:           (&net.Dialer{Timeout: cfg.ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
		TLSHandshakeTimeout:   cfg.TLSTimeout,
		ResponseHeaderTimeout: cfg.HeaderTimeout,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true, // 要的是发行字节本身：不让传输层替我们解压/改写（SHA256 针对原始字节）
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	c := &Client{cfg: cfg, tr: tr}
	c.hc = &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return newErr(KindInsecureURL, "拒绝重定向到非 HTTPS 地址（%s://%s）", req.URL.Scheme, req.URL.Host)
			}
			if len(via) > cfg.MaxRedirects {
				return newErr(KindNetwork, "重定向超过 %d 次", cfg.MaxRedirects)
			}
			return nil
		},
	}
	return c, nil
}

// Close 释放空闲连接。
func (c *Client) Close() { c.tr.CloseIdleConnections() }

// rootPool 返回系统根 + CAFile 的证书池。
func rootPool(caFile string) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if caFile == "" {
		return pool, nil
	}
	st, err := os.Stat(caFile)
	if err != nil {
		return nil, wrapErr(KindOptions, err, "release.ca_file 不可读")
	}
	if !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > maxCAFileBytes {
		return nil, newErr(KindOptions, "release.ca_file 必须是 1 MiB 以内的非空普通文件")
	}
	if st.Mode().Perm()&0o022 != 0 {
		return nil, newErr(KindOptions, "release.ca_file 不得对 group/other 可写（信任锚被篡改的风险）")
	}
	b, err := os.ReadFile(caFile)
	if err != nil {
		return nil, wrapErr(KindOptions, err, "release.ca_file 不可读")
	}
	if !pool.AppendCertsFromPEM(b) {
		return nil, newErr(KindOptions, "release.ca_file 不含任何有效的 PEM 证书")
	}
	return pool, nil
}

// CheckURL 校验 URL：必须是不含凭据的 https。
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || strings.ContainsAny(raw, " \t\r\n") {
		return newErr(KindOptions, "URL 非法")
	}
	if u.Scheme != "https" {
		return newErr(KindInsecureURL, "只允许 https（收到 %q）", u.Scheme)
	}
	if u.User != nil {
		return newErr(KindOptions, "URL 不得含凭据")
	}
	return nil
}

// Expected 是清单对包的承诺：size 同时是下载上限。
type Expected struct {
	Size   int64
	SHA256 string // 64 位小写十六进制
}

var (
	errIdle         = errors.New("响应体长时间无数据")
	errAttemptLimit = errors.New("单次尝试超时")
	errTotalLimit   = errors.New("下载总时长超限")
	reSHA256        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reContentRange  = regexp.MustCompile(`^bytes (\d+)-(\d+)/(\d+)$`)
)

// attempt 是一次 HTTP 尝试的上下文：带单次超时与空闲看门狗。
type attempt struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	idle   *time.Timer
	d      time.Duration
}

func (c *Client) newAttempt(parent context.Context) *attempt {
	ctx, cancel := context.WithCancelCause(parent)
	tctx, tcancel := context.WithTimeoutCause(ctx, c.cfg.AttemptTimeout, errAttemptLimit)
	a := &attempt{ctx: tctx, d: c.cfg.IdleTimeout}
	a.cancel = func(cause error) { tcancel(); cancel(cause) }
	a.idle = time.AfterFunc(a.d, func() { cancel(errIdle) })
	a.idle.Stop() // 空闲看门狗在响应头到达后才开始（等响应头另有 HeaderTimeout）
	return a
}

// arm 启动空闲看门狗（收到响应头之后）。
func (a *attempt) arm() { a.idle.Reset(a.d) }

func (a *attempt) done() { a.idle.Stop(); a.cancel(context.Canceled) }

// idleReader 在每次读到数据后重置空闲计时器。
type idleReader struct {
	r io.Reader
	a *attempt
}

func (r idleReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.a.idle.Reset(r.a.d)
	}
	return n, err
}

// classify 把一次失败分类：是否可重试，以及稳定的 *Error。parent 已结束（调用方取消/总超时）时不再重试。
func (c *Client) classify(parent context.Context, a *attempt, err error) (retry bool, out error) {
	var de *Error
	if errors.As(err, &de) {
		return false, de // 策略类失败（重定向降级、大小、SHA…）：不重试
	}
	var cve *tls.CertificateVerificationError
	var uae x509.UnknownAuthorityError
	var hne x509.HostnameError
	var cie x509.CertificateInvalidError
	if errors.As(err, &cve) || errors.As(err, &uae) || errors.As(err, &hne) || errors.As(err, &cie) {
		return false, wrapErr(KindTLS, err, "服务器证书校验失败（不在系统根证书/release.ca_file 中，或主机名/有效期不符）")
	}
	if cause := context.Cause(parent); cause != nil {
		if errors.Is(cause, errTotalLimit) {
			return false, wrapErr(KindNetwork, cause, "下载总时长超过 %s", c.cfg.TotalTimeout)
		}
		return false, cause // 调用方取消（Ctrl-C）：原样返回，不改写
	}
	if a != nil {
		if cause := context.Cause(a.ctx); cause != nil {
			switch {
			case errors.Is(cause, errIdle):
				return true, wrapErr(KindNetwork, cause, "响应体超过 %s 无数据", c.cfg.IdleTimeout)
			case errors.Is(cause, errAttemptLimit):
				return true, wrapErr(KindNetwork, cause, "单次尝试超过 %s", c.cfg.AttemptTimeout)
			}
		}
	}
	return true, wrapErr(KindNetwork, err, "传输失败")
}

func (c *Client) backoff(ctx context.Context, n int) error {
	d := c.cfg.BackoffBase << (n - 1)
	if d > c.cfg.BackoffMax || d <= 0 {
		d = c.cfg.BackoffMax
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		return nil
	}
}

// retrying 以有限重试包裹 fn；fn 返回 (是否可重试, 错误)。
func (c *Client) retrying(ctx context.Context, fn func(ctx context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeoutCause(ctx, c.cfg.TotalTimeout, errTotalLimit)
	defer cancel()
	var last error
	for n := 1; n <= c.cfg.MaxAttempts; n++ {
		retry, err := fn(ctx)
		if err == nil {
			return nil
		}
		last = err
		if !retry || n == c.cfg.MaxAttempts {
			break
		}
		if c.cfg.OnRetry != nil {
			c.cfg.OnRetry(n, err)
		}
		if berr := c.backoff(ctx, n); berr != nil {
			_, out := c.classify(ctx, nil, berr)
			return out
		}
	}
	var de *Error
	if errors.As(last, &de) && de.Kind == KindNetwork {
		_ = de.with("attempts", c.cfg.MaxAttempts)
	}
	return last
}

func (c *Client) get(ctx context.Context, raw string, offset int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, newErr(KindOptions, "URL 非法")
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Encoding", "identity")
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	return c.hc.Do(req)
}

// statusErr 把非预期的 HTTP 状态映射为 (是否可重试, 错误)。
func statusErr(resp *http.Response) (bool, error) {
	code := resp.StatusCode
	retry := code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500
	return retry, newErr(KindNetwork, "HTTP %d", code).with("status", code)
}

// FetchBytes 取小对象（manifest.json/.sig）：只 HTTPS，大小上限 max（声明与实际流都检查），有限重试。
func (c *Client) FetchBytes(ctx context.Context, raw string, max int64) ([]byte, error) {
	if err := CheckURL(raw); err != nil {
		return nil, err
	}
	var out []byte
	err := c.retrying(ctx, func(ctx context.Context) (bool, error) {
		a := c.newAttempt(ctx)
		defer a.done()
		resp, err := c.get(a.ctx, raw, 0)
		if err != nil {
			return c.classify(ctx, a, err)
		}
		defer func() { _ = resp.Body.Close() }()
		a.arm()
		if resp.StatusCode != http.StatusOK {
			return statusErr(resp)
		}
		if resp.ContentLength > max {
			return false, newErr(KindSize, "声明大小 %d 超过上限 %d", resp.ContentLength, max)
		}
		b, err := io.ReadAll(io.LimitReader(idleReader{resp.Body, a}, max+1))
		if err != nil {
			return c.classify(ctx, a, err)
		}
		if int64(len(b)) > max {
			return false, newErr(KindSize, "实际流超过上限 %d", max)
		}
		if resp.ContentLength >= 0 && int64(len(b)) != resp.ContentLength {
			return true, newErr(KindSize, "响应体被截断")
		}
		out = b
		return false, nil
	})
	return out, err
}

// Download 下载 raw 到 dst：大小上限 = exp.Size（Content-Length 声明与实际流都检查），流式 SHA256，
// 失败可经 Range 断点续传（已有 .part 先从磁盘重新计算哈希，不信任内存状态）。
// 整个文件的长度与 SHA256 全部通过之后才把 dst+".part" 改名为 dst（「校验后合并」）：任何一段被篡改（含续传的后半段）都使整体失败并删除 .part。
func (c *Client) Download(ctx context.Context, raw, dst string, exp Expected) error {
	if err := CheckURL(raw); err != nil {
		return err
	}
	if exp.Size < 1 || !reSHA256.MatchString(exp.SHA256) {
		return newErr(KindOptions, "Expected 的 size/sha256 非法")
	}
	part := dst + ".part"
	var slot *resumeSlot
	if c.cfg.ResumeDir != "" {
		var err error
		if slot, err = c.openResume(raw, exp); err != nil {
			return err
		}
	}
	if slot != nil { // 缓存槽位：partial 在 staging 之外，可能来自上一次调用（不被信任，见 resume.go）
		defer slot.release()
		part = slot.path
	} else if err := os.Remove(part); err != nil && !errors.Is(err, os.ErrNotExist) {
		return wrapErr(KindStaging, err, "无法清理残留的 .part")
	}
	var lastRetryable bool
	err := c.retrying(ctx, func(ctx context.Context) (bool, error) {
		retry, err := c.downloadOnce(ctx, raw, part, exp)
		lastRetryable = retry && err != nil
		return retry, err
	})
	if err != nil {
		if slot == nil || !keepPartial(err, lastRetryable) {
			_ = os.Remove(part)
		}
		return err
	}
	if slot != nil {
		return finalizeResume(part, dst, exp)
	}
	if err := os.Rename(part, dst); err != nil {
		_ = os.Remove(part)
		return wrapErr(KindStaging, err, "无法落名下载结果")
	}
	return nil
}

func (c *Client) downloadOnce(ctx context.Context, raw, part string, exp Expected) (bool, error) {
	var offset int64
	h := sha256.New()
	if st, err := os.Lstat(part); err == nil {
		if !st.Mode().IsRegular() {
			return false, newErr(KindStaging, ".part 不是普通文件")
		}
		offset = st.Size()
		if offset > exp.Size {
			if err := os.Remove(part); err != nil {
				return false, wrapErr(KindStaging, err, "无法清理 .part")
			}
			offset = 0
		}
	}
	if offset > 0 { // 从磁盘重新计算已有部分的哈希
		f, err := openNoFollow(part, os.O_RDONLY)
		if err != nil {
			return false, wrapErr(KindStaging, err, "无法读取 .part")
		}
		n, err := io.Copy(h, io.LimitReader(f, offset))
		_ = f.Close()
		if err != nil || n != offset {
			return false, newErr(KindStaging, "无法读取 .part")
		}
	}
	if offset < exp.Size {
		retry, err := c.fetchInto(ctx, raw, part, exp, &offset, h)
		if err != nil {
			return retry, err
		}
	}
	// 此时 .part 恰有 exp.Size 字节：校验整体 SHA256。
	if got := hex.EncodeToString(h.Sum(nil)); got != exp.SHA256 {
		return false, newErr(KindSHA256, "包的 SHA256 与清单不符").with("expected", exp.SHA256).with("actual", got)
	}
	return false, nil
}

// fetchInto 发一次（可能带 Range 的）请求，把响应体追加写入 part 并更新 offset/h。
func (c *Client) fetchInto(ctx context.Context, raw, part string, exp Expected, offset *int64, h hash.Hash) (bool, error) {
	a := c.newAttempt(ctx)
	defer a.done()
	resp, err := c.get(a.ctx, raw, *offset)
	if err != nil {
		return c.classify(ctx, a, err)
	}
	defer func() { _ = resp.Body.Close() }()
	a.arm()

	truncate := false
	switch resp.StatusCode {
	case http.StatusOK:
		truncate = *offset > 0 // 服务器无视 Range，整包重来
	case http.StatusPartialContent:
		m := reContentRange.FindStringSubmatch(resp.Header.Get("Content-Range"))
		if *offset == 0 || m == nil {
			return false, newErr(KindSize, "非预期的 206/Content-Range 响应")
		}
		start, _ := strconv.ParseInt(m[1], 10, 64)
		end, _ := strconv.ParseInt(m[2], 10, 64)
		total, _ := strconv.ParseInt(m[3], 10, 64)
		if start != *offset || total != exp.Size || end != exp.Size-1 {
			return false, newErr(KindSize, "Content-Range 与请求/清单不符").with("content_range", resp.Header.Get("Content-Range"))
		}
	case http.StatusRequestedRangeNotSatisfiable:
		// .part 与服务器状态对不上：丢弃已有部分，重新开始（算一次尝试）。
		_ = os.Remove(part)
		*offset = 0
		h.Reset()
		return true, newErr(KindNetwork, "HTTP 416，丢弃已有部分后重试").with("status", 416)
	default:
		return statusErr(resp)
	}
	if truncate {
		*offset = 0
		h.Reset()
	}
	want := exp.Size - *offset
	if resp.ContentLength > want {
		return false, newErr(KindSize, "声明大小 %d 超过清单 size 允许的剩余 %d", resp.ContentLength, want)
	}
	if resp.ContentLength >= 0 && resp.ContentLength < want {
		return false, newErr(KindSize, "声明大小 %d 小于清单 size 要求的 %d（截断）", resp.ContentLength, want)
	}

	var f *os.File
	if *offset == 0 {
		_ = os.Remove(part)
		f, err = security.CreateFile(part, 0o600)
	} else {
		f, err = openNoFollow(part, os.O_WRONLY|os.O_APPEND)
	}
	if err != nil {
		return false, wrapErr(KindStaging, err, "无法打开 .part")
	}
	n, cerr := io.Copy(io.MultiWriter(f, h), io.LimitReader(idleReader{resp.Body, a}, want+1))
	*offset += min(n, want)
	if n > want { // 实际流超过清单 size：中止（不信任 Content-Length）
		_ = f.Close()
		return false, newErr(KindSize, "实际流超过清单 size %d", exp.Size)
	}
	if serr := f.Sync(); serr != nil && cerr == nil {
		cerr = serr
	}
	if clerr := f.Close(); clerr != nil && cerr == nil {
		cerr = clerr
	}
	if cerr != nil {
		return c.classify(ctx, a, cerr)
	}
	if n < want { // 干净 EOF 但不足：截断，保留已写部分供续传
		return true, newErr(KindSize, "响应体被截断：收到 %d / %d 字节", *offset, exp.Size)
	}
	return false, nil
}

// HashFile 流式计算文件的 SHA256 与长度（离线路径与测试共用）。
func HashFile(path string) (sum string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func (e Expected) String() string { return fmt.Sprintf("%d bytes sha256:%s", e.Size, e.SHA256) }
