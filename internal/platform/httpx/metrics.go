package httpx

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultMetricsAddr 是 Prometheus 指标监听地址（9180：避开 node_exporter 常用的 9100，architecture §7 / tools-design §10.4）。
const DefaultMetricsAddr = "127.0.0.1:9180"

var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type reqKey struct {
	method, route string
	status        int
}

type durKey struct{ method, route string }

type hist struct {
	counts [11]uint64 // 与 durationBuckets 一一对应（非累计，输出时累加）
	inf    uint64     // 超过最大桶
	sum    float64
	n      uint64
}

type gaugeFn struct {
	name, help string
	fn         func() float64
}

// Metrics 是进程内的 Prometheus 文本指标（不引入客户端库：只需计数器、直方图与少量仪表）。route 标签是路由模板（有界基数），
// 未命中路由统一记为 "unmatched"，绝不使用原始路径。
type Metrics struct {
	mu     sync.Mutex
	reqs   map[reqKey]uint64
	durs   map[durKey]*hist
	gauges []gaugeFn

	inflight atomic.Int64
	panics   atomic.Uint64
	replays  atomic.Uint64
	reuses   atomic.Uint64
}

// NewMetrics 创建指标集合。
func NewMetrics() *Metrics {
	return &Metrics{reqs: map[reqKey]uint64{}, durs: map[durKey]*hist{}}
}

var reMetricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// RegisterGauge 注册一个在每次抓取时求值的仪表（如连接池占用、Redis 降级状态）。名称非法会 panic（编程错误，启动期暴露）。
func (m *Metrics) RegisterGauge(name, help string, fn func() float64) {
	if !reMetricName.MatchString(name) || fn == nil {
		panic("httpx: 非法的指标注册: " + name)
	}
	m.mu.Lock()
	m.gauges = append(m.gauges, gaugeFn{name, help, fn})
	m.mu.Unlock()
}

func (m *Metrics) observe(method, route string, status int, d time.Duration) {
	sec := d.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reqs[reqKey{method, route, status}]++
	h := m.durs[durKey{method, route}]
	if h == nil {
		h = &hist{}
		m.durs[durKey{method, route}] = h
	}
	h.sum += sec
	h.n++
	for i, b := range durationBuckets {
		if sec <= b {
			h.counts[i]++
			return
		}
	}
	h.inf++
}

func escLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// WriteTo 以 Prometheus 文本格式（0.0.4）输出全部指标，序稳定。
func (m *Metrics) WriteTo(w io.Writer) (int64, error) {
	var bw bytes.Buffer // 写 bytes.Buffer 不会失败；最后一次性写出
	m.mu.Lock()
	reqs := make([]reqKey, 0, len(m.reqs))
	for k := range m.reqs {
		reqs = append(reqs, k)
	}
	sort.Slice(reqs, func(i, j int) bool {
		a, b := reqs[i], reqs[j]
		if a.route != b.route {
			return a.route < b.route
		}
		if a.method != b.method {
			return a.method < b.method
		}
		return a.status < b.status
	})
	durs := make([]durKey, 0, len(m.durs))
	for k := range m.durs {
		durs = append(durs, k)
	}
	sort.Slice(durs, func(i, j int) bool {
		if durs[i].route != durs[j].route {
			return durs[i].route < durs[j].route
		}
		return durs[i].method < durs[j].method
	})
	fmt.Fprintln(&bw, "# HELP lingedge_http_requests_total HTTP 请求总数（route 为路由模板）。")
	fmt.Fprintln(&bw, "# TYPE lingedge_http_requests_total counter")
	for _, k := range reqs {
		fmt.Fprintf(&bw, "lingedge_http_requests_total{method=\"%s\",route=\"%s\",status=\"%d\"} %d\n", escLabel(k.method), escLabel(k.route), k.status, m.reqs[k])
	}
	fmt.Fprintln(&bw, "# HELP lingedge_http_request_duration_seconds HTTP 请求耗时（秒）。")
	fmt.Fprintln(&bw, "# TYPE lingedge_http_request_duration_seconds histogram")
	for _, k := range durs {
		h := m.durs[k]
		lbl := fmt.Sprintf("method=\"%s\",route=\"%s\"", escLabel(k.method), escLabel(k.route))
		var cum uint64
		for i, b := range durationBuckets {
			cum += h.counts[i]
			fmt.Fprintf(&bw, "lingedge_http_request_duration_seconds_bucket{%s,le=\"%s\"} %d\n", lbl, strconv.FormatFloat(b, 'g', -1, 64), cum)
		}
		fmt.Fprintf(&bw, "lingedge_http_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", lbl, h.n)
		fmt.Fprintf(&bw, "lingedge_http_request_duration_seconds_sum{%s} %s\n", lbl, strconv.FormatFloat(h.sum, 'g', -1, 64))
		fmt.Fprintf(&bw, "lingedge_http_request_duration_seconds_count{%s} %d\n", lbl, h.n)
	}
	gauges := append([]gaugeFn(nil), m.gauges...)
	m.mu.Unlock()

	scalar := func(name, typ, help string, v string) {
		fmt.Fprintf(&bw, "# HELP %s %s\n# TYPE %s %s\n%s %s\n", name, help, name, typ, name, v)
	}
	scalar("lingedge_http_requests_in_flight", "gauge", "正在处理的 HTTP 请求数。", strconv.FormatInt(m.inflight.Load(), 10))
	scalar("lingedge_http_panics_total", "counter", "被 recover 捕获的 handler panic 次数。", strconv.FormatUint(m.panics.Load(), 10))
	scalar("lingedge_idempotency_replays_total", "counter", "Idempotency-Key 重放（返回首次响应）次数。", strconv.FormatUint(m.replays.Load(), 10))
	scalar("lingedge_idempotency_key_reuse_total", "counter", "Idempotency-Key 异体复用（422）次数。", strconv.FormatUint(m.reuses.Load(), 10))
	for _, g := range gauges {
		scalar(g.name, "gauge", strings.ReplaceAll(g.help, "\n", " "), strconv.FormatFloat(g.fn(), 'g', -1, 64))
	}
	n, err := w.Write(bw.Bytes())
	return int64(n), err
}

// Handler 是 /metrics 处理器。
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = m.WriteTo(w)
	})
}

// NewMetricsServer 构造只服务 GET /metrics 的 http.Server（监听/关闭由进程装配负责，Story 1.13）。
// addr 通常取 DefaultMetricsAddr（127.0.0.1:9180）；指标不含任何用户数据，但仍不应暴露到公网。
func NewMetricsServer(addr string, m *Metrics) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	return &http.Server{
		Addr: addr, Handler: mux,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}
}
