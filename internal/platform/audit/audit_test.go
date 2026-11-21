package audit

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

func TestCatalogIsConsistent(t *testing.T) {
	ops, acts := map[string]bool{}, map[Action]string{}
	for _, o := range Catalog {
		if o.OperationID == "" || len(o.Actions) == 0 {
			t.Errorf("目录条目不完整: %+v", o)
		}
		if ops[o.OperationID] {
			t.Errorf("operation %s 重复登记", o.OperationID)
		}
		ops[o.OperationID] = true
		for _, a := range o.Actions {
			if !strings.Contains(string(a), ".") {
				t.Errorf("动作 %q 应为 <资源>.<动作> 形态", a)
			}
			acts[a] = o.OperationID
		}
	}
	// 动作取值按目录 operation 的字典序稳定（便于评审）：这里只核对每个动作都能被 knownAction 识别、未登记的不被识别。
	var names []string
	for a := range acts {
		if !knownAction(a) {
			t.Errorf("knownAction 应识别 %q", a)
		}
		names = append(names, string(a))
	}
	sort.Strings(names)
	if knownAction("no.such_action") || Registered("noSuchOperation") || !Registered("createUser") || ActionsFor("noSuchOperation") != nil {
		t.Error("Registered/ActionsFor/knownAction 判定不正确")
	}
	if len(ops) != 27 {
		t.Errorf("应登记 27 个 operation: %d", len(ops))
	}
}

// 审计钩子的运行期兜底：只有「声明了审计、成功、非重放、未 panic、却没有审计行」才报警并计数；未登记的 x-audit operation 在 Begin 即拒绝。
func TestAuditorMissingDetection(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	var reported []string
	w := &Writer{log: zap.New(core), onMissing: func(op string) { reported = append(reported, op) }, extra: []Op{{OperationID: "probeOp", Actions: []Action{ActionTaskCancel}}}}
	au := w.Auditor()
	r := httptest.NewRequest(http.MethodPost, "/x", nil)

	if end, err := au.Begin(r, httpx.RouteMeta{OperationID: "listThings"}); end != nil || err != nil {
		t.Fatalf("Audit=false 的路由不处理: %v", err)
	}
	if _, err := au.Begin(r, httpx.RouteMeta{OperationID: "notRegisteredOp", Audit: true}); err == nil {
		t.Fatal("未登记的 x-audit operation 必须 fail-closed 拒绝")
	}
	for _, op := range []string{"createUser", "probeOp"} {
		if _, err := au.Begin(r, httpx.RouteMeta{OperationID: op, Audit: true}); err != nil {
			t.Fatalf("%s 已登记（含 ExtraOps）应放行: %v", op, err)
		}
	}
	end, _ := au.Begin(r, httpx.RouteMeta{OperationID: "createUser", Audit: true})
	for name, o := range map[string]httpx.AuditOutcome{
		"已记录": {Status: 201, Recorded: 1}, "幂等重放": {Status: 201, Replayed: true}, "panic": {Status: 500, Panicked: true},
		"业务失败 409": {Status: 409}, "业务失败 500": {Status: 500}, "未认证 401": {Status: 401},
	} {
		end(o)
		if w.Missing() != 0 {
			t.Fatalf("%s 不应报警", name)
		}
	}
	for _, st := range []int{200, 201, 204} {
		end(httpx.AuditOutcome{Status: st})
	}
	if w.Missing() != 3 || len(reported) != 3 || reported[0] != "createUser" || logs.Len() != 3 {
		t.Fatalf("成功却没有审计行应报警 3 次: missing=%d reported=%v logs=%d", w.Missing(), reported, logs.Len())
	}
}

func TestIPBytesAndTrunc(t *testing.T) {
	if b := ipBytes("10.1.2.3"); len(b) != 4 {
		t.Errorf("IPv4 应为 4 字节: %v", b)
	}
	if b := ipBytes("2001:db8::1"); len(b) != 16 {
		t.Errorf("IPv6 应为 16 字节: %v", b)
	}
	if ipBytes("") != nil || ipBytes("not-an-ip") != nil {
		t.Error("空/非法 IP 应为 nil")
	}
	if got := trunc("审计日志", 7); got != "审计" || trunc("abc", 5) != "abc" {
		t.Errorf("trunc 不得截断 UTF-8 多字节序列: %q", got)
	}
}
