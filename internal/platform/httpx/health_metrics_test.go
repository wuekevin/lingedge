package httpx_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

func TestHealthzLivenessNeedsNoDependencies(t *testing.T) {
	r, _ := newRouter(t, httpx.Options{})
	rec := do(handler(t, r), "GET", "/healthz", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"ok"`) || rec.Header().Get("X-Request-ID") == "" {
		t.Fatalf("/healthz: %d %s", rec.Code, rec.Body)
	}
}

// 依赖未装配时就绪必须失败（不能默认通过）；Redis 未装配只降级。
func TestReadyFailsClosedWithoutDependencies(t *testing.T) {
	r, _ := newRouter(t, httpx.Options{})
	rec := do(handler(t, r), "GET", "/healthz/ready", "")
	var rep httpx.ReadyReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 503 || rep.Status != httpx.StatusUnavailable || rep.Checks["database"].Status != "unconfigured" || rep.Checks["redis"].Status != "unconfigured" {
		t.Fatalf("未装配依赖时就绪应 503: %d %+v", rec.Code, rep)
	}
	if rep.Checks["migrations"].Status != "unknown" {
		t.Errorf("数据库不可用时迁移状态应为 unknown: %+v", rep.Checks["migrations"])
	}
}

// 指标：Prometheus 文本、route 为模板、127.0.0.1 监听、仅 GET /metrics。
func TestMetricsPrometheusText(t *testing.T) {
	oh := &orderHooks{p: &testPrincipal{actor: 1, tenant: 1}}
	r, _ := newRouter(t, httpx.Options{Hooks: oh.hooks()})
	if err := r.Register(meta("getSite", "GET", "/sites/{siteId}"), okHandler("x")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(meta("boom", "GET", "/boom"), func(c *gin.Context) { panic("x") }); err != nil {
		t.Fatal(err)
	}
	h := handler(t, r)
	do(h, "GET", "/api/v1/sites/S1", "")
	do(h, "GET", "/api/v1/sites/S2", "")
	do(h, "GET", "/api/v1/boom", "")
	do(h, "GET", "/no/such/thing/123", "")
	r.Metrics().RegisterGauge("lingedge_test_gauge", "测试仪表。", func() float64 { return 3 })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httpx.NewMetricsServer(ln.Addr().String(), r.Metrics())
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	resp, err := http.Get("http://" + ln.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	text := string(b)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("/metrics: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{
		"# TYPE lingedge_http_requests_total counter",
		`lingedge_http_requests_total{method="GET",route="/api/v1/sites/:siteId",status="200"} 2`,
		`lingedge_http_requests_total{method="GET",route="/api/v1/boom",status="500"} 1`,
		`lingedge_http_requests_total{method="GET",route="unmatched",status="404"} 1`,
		"# TYPE lingedge_http_request_duration_seconds histogram",
		`lingedge_http_request_duration_seconds_count{method="GET",route="/api/v1/sites/:siteId"} 2`,
		`lingedge_http_request_duration_seconds_bucket{method="GET",route="/api/v1/sites/:siteId",le="+Inf"} 2`,
		"lingedge_http_panics_total 1",
		"lingedge_http_requests_in_flight 0",
		"lingedge_test_gauge 3",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("指标缺少 %q\n%s", want, text)
		}
	}
	if strings.Contains(text, "S1") || strings.Contains(text, "/no/such") {
		t.Errorf("route 标签不得含原始路径（基数爆炸/泄漏）: %s", text)
	}
	// 每个样本行都符合 Prometheus 文本格式。
	sample := regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{([a-zA-Z_][a-zA-Z0-9_]*="[^"\\]*(\\.[^"\\]*)*",?)*\})? [-+0-9.eE]+(Inf)?$`)
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if !sample.MatchString(line) {
			t.Errorf("非法的 Prometheus 样本行: %q", line)
		}
	}
	// 仅服务 GET /metrics。
	for _, c := range []struct{ method, path string }{{"POST", "/metrics"}, {"GET", "/"}, {"GET", "/healthz"}} {
		req, _ := http.NewRequest(c.method, "http://"+ln.Addr().String()+c.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Errorf("%s %s 不应被指标服务接受", c.method, c.path)
		}
	}
	if httpx.DefaultMetricsAddr != "127.0.0.1:9180" {
		t.Errorf("默认指标地址应为 127.0.0.1:9180: %s", httpx.DefaultMetricsAddr)
	}
}
