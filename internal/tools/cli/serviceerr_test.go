package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/service"
)

// ServiceError 必须覆盖 service 的每一种分类（新增分类而忘记映射会使本测试失败）。
func TestServiceErrorMappingIsExhaustive(t *testing.T) {
	want := map[service.Kind]struct {
		exit ExitCode
		code string
	}{
		service.KindInvalidParam: {ExitGeneral, "service.invalid_param"},
		service.KindNotSystemd:   {ExitPrecondition, "precondition.systemd"},
		service.KindUnsupported:  {ExitPrecondition, "precondition.systemd_version"},
		service.KindExec:         {ExitGeneral, "service.exec_failed"},
		service.KindIO:           {ExitPrecondition, "service.io"},
		service.KindConflict:     {ExitPrecondition, "service.conflict"},
		service.KindPermission:   {ExitPrecondition, "precondition.root"},
		service.KindUndo:         {ExitGeneral, "service.undo_refused"},
	}
	for _, k := range service.Kinds() {
		w, ok := want[k]
		if !ok {
			t.Fatalf("分类 %s 没有声明预期映射", k)
		}
		e := ServiceError(fmt.Errorf("包装: %w", &service.Error{Kind: k, Msg: "m", Path: "/p"}))
		if e == nil || e.Exit != w.exit || e.Code != w.code || e.Hint == "" || e.Message != "m" || e.Details["path"] != "/p" || e.SystemState != "unchanged" {
			t.Errorf("%s -> %+v，期望 exit=%d code=%s", k, e, w.exit, w.code)
		}
	}
	if len(want) != len(service.Kinds()) {
		t.Errorf("映射表 %d 项，service.Kinds() %d 项", len(want), len(service.Kinds()))
	}
}

func TestServiceErrorMismatchAndPassthrough(t *testing.T) {
	e := ServiceError(&service.MismatchError{Items: []string{"/a 权限 0777 应为 0750", "/b 属主 1:1 应为 x:y"}})
	if e == nil || e.Exit != ExitPrecondition || e.Code != "service.dir_mismatch" {
		t.Fatalf("%+v", e)
	}
	if items, _ := e.Details["items"].([]any); len(items) != 2 {
		t.Errorf("details.items 须是 []any（envelope 脱敏只遍历动态类型）：%T", e.Details["items"])
	}
	if ServiceError(errors.New("other")) != nil {
		t.Error("不认识的错误应返回 nil")
	}
	pre := &Error{Exit: ExitUsage, Code: "usage.x"}
	if ServiceError(pre) != pre {
		t.Error("已是 *Error 的应原样返回")
	}
}

// service 错误必须被识别为命令执行期的结构化错误（否则会被当成 Cobra 解析阶段错误并映射成用法错误 2）。
func TestServiceErrorIsNotParseStageError(t *testing.T) {
	for _, k := range service.Kinds() {
		if isParseStageError(fmt.Errorf("w: %w", &service.Error{Kind: k, Msg: "m"})) {
			t.Errorf("%s 被误判为解析阶段错误", k)
		}
	}
	if isParseStageError(&service.MismatchError{Items: []string{"x"}}) {
		t.Error("MismatchError 被误判为解析阶段错误")
	}
}
