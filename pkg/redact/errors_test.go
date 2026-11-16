package redact

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

type sentinelErr struct{ code int }

func (e *sentinelErr) Error() string { return fmt.Sprintf("sentinel %d", e.code) }

// hidingErr 的 Error() 不含被包装原因，原因只能经 Unwrap 取得。
type hidingErr struct{ cause error }

func (e *hidingErr) Error() string { return "operation failed" }
func (e *hidingErr) Unwrap() error { return e.cause }

func TestErrorWrapsAndKeepsErrorsIsAs(t *testing.T) {
	const leak = "ErrChainPassw0rd"
	base := &sentinelErr{code: 7}
	err := fmt.Errorf("connect lingedge_app:%s@tcp(h:1)/d: %w", leak, base)
	w := Error(err)

	if strings.Contains(w.Error(), leak) {
		t.Fatalf("错误文本泄漏: %q", w.Error())
	}
	if !strings.Contains(w.Error(), Mask) {
		t.Errorf("应含 Mask: %q", w.Error())
	}
	var got *sentinelErr
	if !errors.As(w, &got) || got.code != 7 {
		t.Error("errors.As 应仍能取到原始错误")
	}
	if !errors.Is(w, base) || !errors.Is(Error(fmt.Errorf("x: %w", fs.ErrNotExist)), fs.ErrNotExist) {
		t.Error("errors.Is 应仍可用")
	}
	if Error(nil) != nil {
		t.Error("nil 应返回 nil")
	}
	if Error(w) != w {
		t.Error("已清洗的错误应原样返回（幂等）")
	}
}

func TestChainScrubsEveryLayerAndJoinBranches(t *testing.T) {
	const a, b, c = "LayerSecretA9", "LayerSecretB9", "LayerSecretC9"
	inner := errors.New("dial redis://:" + a + "@h:6379/0")
	hid := &hidingErr{cause: fmt.Errorf("token le_chain001_%s rejected: %w", b, inner)}
	joined := errors.Join(hid, errors.New("password="+c))

	msgs := Chain(joined)
	all := strings.Join(msgs, "\n")
	for _, s := range []string{a, b, c} {
		if strings.Contains(all, s) {
			t.Errorf("错误链泄漏 %q:\n%s", s, all)
		}
	}
	if len(msgs) < 4 {
		t.Errorf("应逐层列出（含被 Error() 隐藏的原因与 Join 分支），实际 %d 层: %q", len(msgs), msgs)
	}
	if Chain(nil) != nil {
		t.Error("nil 错误链应为空")
	}
}

type panicStringer struct{}

func (panicStringer) String() string { panic("boom password=stringer-leak-1") }

type okStringer struct{}

func (okStringer) String() string { return "stringer password=stringer-leak-2" }

func TestPanicValuesAreScrubbed(t *testing.T) {
	cases := []struct {
		name    string
		v       any
		secret  string
		wantSub string
	}{
		{"string", "failed: password=panic-leak-1", "panic-leak-1", Mask},
		{"error", fmt.Errorf("dial u:%s@tcp(h:1)/d", "panicDsnPass"), "panicDsnPass", Mask},
		{"stringer", okStringer{}, "stringer-leak-2", Mask},
		{"panicking stringer", panicStringer{}, "stringer-leak-1", "panic in String"},
		{"struct", struct{ Token string }{"Token: tokenleakvalue99"}, "tokenleakvalue99", Mask},
		{"map", map[string]string{"k": "le_pan00001_PanicTokenSecret77"}, "PanicTokenSecret77", Mask},
		{"nil", nil, "", "<nil>"},
	}
	for _, tc := range cases {
		got := Panic(tc.v)
		if tc.secret != "" && strings.Contains(got, tc.secret) {
			t.Errorf("%s: panic 值泄漏 %q: %q", tc.name, tc.secret, got)
		}
		if !strings.Contains(got, tc.wantSub) {
			t.Errorf("%s: 输出应含 %q: %q", tc.name, tc.wantSub, got)
		}
	}
}

func TestStackIsScrubbedAndNonEmpty(t *testing.T) {
	s := Stack()
	if !strings.Contains(s, "TestStackIsScrubbedAndNonEmpty") {
		t.Errorf("栈应含调用函数名: %q", s)
	}
}

type cyclicErr struct{}

func (e *cyclicErr) Error() string { return "cyclic" }
func (e *cyclicErr) Unwrap() error { return e }

type panicUnwrapErr struct{}

func (panicUnwrapErr) Error() string { return "outer " + "pass" + "word=PanicUnwrapLeak1" }
func (panicUnwrapErr) Unwrap() error { panic("unwrap boom") }

func TestChainSurvivesCyclesAndPanickingUnwrap(t *testing.T) {
	if got := Chain(&cyclicErr{}); len(got) != 1 || got[0] != "cyclic" {
		t.Errorf("自引用 Unwrap 应在有限深度内结束: %v", got)
	}
	got := strings.Join(Chain(panicUnwrapErr{}), "|")
	if strings.Contains(got, "PanicUnwrapLeak1") || !strings.Contains(got, "outer") {
		t.Errorf("Unwrap panic 时仍应输出已清洗的外层: %q", got)
	}
}
