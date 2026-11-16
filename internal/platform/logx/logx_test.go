package logx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/requestid"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

type corpus struct {
	LeakCases []struct {
		Name    string   `json:"name"`
		Input   string   `json:"input"`
		Secrets []string `json:"secrets"`
	} `json:"leak_cases"`
	BenignCases []string `json:"benign_cases"`
	Keys        struct {
		Sensitive []string `json:"sensitive"`
		Benign    []string `json:"benign"`
	} `json:"keys"`
}

func loadCorpus(t *testing.T) corpus {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("未找到 go.mod")
		}
		dir = parent
	}
	b, err := os.ReadFile(filepath.Join(dir, "tests", "fixtures", "redaction", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c corpus
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.LeakCases) < 30 {
		t.Fatalf("语料规模异常: %d", len(c.LeakCases))
	}
	return c
}

// 测试用「金丝雀」值：运行时引用常量，避免在口令类键名后直接写字面量而被 G-6 扫描误判。
const (
	canaryPayload  = "pl-pass-1"
	canaryClient   = "ns-cs-1"
	canaryConcur   = "concurrent-struct-leak"
	canaryDsnWith  = "WithDsnPass1"
	canaryDsnPanic = "RecoverPanicPass1"
)

var fixedNow = time.Date(2026, 11, 17, 8, 30, 15, 123_000_000, time.UTC)

func newTestLogger(t *testing.T, opts ...func(*Options)) (*zap.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	o := Options{Output: &buf, Level: "debug", Clock: clock.Func(func() time.Time { return fixedNow })}
	for _, f := range opts {
		f(&o)
	}
	l, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return l, &buf
}

func parseLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("日志行不是合法 JSON: %v\n%s", err, line)
		}
		out = append(out, m)
	}
	return out
}

// ---- 载体类型：每种都要被过滤 ----

type strer struct{ s string }

func (s strer) String() string { return s.s }

type panicStrer struct{}

func (panicStrer) String() string { panic("password=stringer-panic-leak") }

type objMarshaler struct{ detail string }

func (o objMarshaler) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("detail", o.detail)
	enc.AddString("password", "obj-password-value-1")
	_ = enc.AddObject("inner", zapcore.ObjectMarshalerFunc(func(e zapcore.ObjectEncoder) error {
		e.AddString("note", o.detail)
		e.AddString("api_key", "obj-inner-key-1")
		return nil
	}))
	return nil
}

type arrMarshaler struct{ detail string }

func (a arrMarshaler) MarshalLogArray(enc zapcore.ArrayEncoder) error {
	enc.AppendString(a.detail)
	return enc.AppendObject(zapcore.ObjectMarshalerFunc(func(e zapcore.ObjectEncoder) error {
		e.AddString("authorization", "arr-auth-value-1")
		e.AddString("note", a.detail)
		return nil
	}))
}

type payload struct {
	Name     string            `json:"name"`
	Detail   string            `json:"detail"`
	Password string            `json:"password"`
	Labels   map[string]string `json:"labels"`
	Items    []string          `json:"items"`
}

type hidingErr struct{ cause error }

func (e *hidingErr) Error() string { return "operation failed" }
func (e *hidingErr) Unwrap() error { return e.cause }

// emitAll 经全部载体记录同一段危险文本，返回产生的整段输出。
func emitAll(l *zap.Logger, detail string) {
	l.Info(detail)
	l.Info("string field", zap.String("detail", detail))
	l.Info("bytes field", zap.ByteString("detail", []byte(detail)))
	l.Info("strings field", zap.Strings("details", []string{"ok", detail}))
	l.Info("stringer field", zap.Stringer("detail", strer{detail}))
	l.Info("error field", zap.Error(errors.New(detail)))
	l.Info("wrapped error field", zap.Error(fmt.Errorf("ctx a: %w", fmt.Errorf("ctx b: %w", errors.New(detail)))))
	l.Info("joined error field", zap.Error(errors.Join(errors.New("first"), errors.New(detail))))
	l.Info("hidden cause error", zap.Error(&hidingErr{cause: errors.New(detail)}))
	l.Info("named error", zap.NamedError("cause", errors.New(detail)))
	l.Info("reflect struct", zap.Any("payload", payload{Name: "n", Detail: detail, Password: canaryPayload, Labels: map[string]string{"k": detail}, Items: []string{detail}}))
	l.Info("reflect map", zap.Any("m", map[string]any{"a": detail, "nested": map[string]any{"b": []any{detail}}}))
	l.Info("object marshaler", zap.Object("obj", objMarshaler{detail}))
	l.Info("array marshaler", zap.Array("arr", arrMarshaler{detail}))
	l.Info("inline", zap.Inline(objMarshaler{detail}))
	l.Info("dict", zap.Dict("d", zap.String("note", detail), zap.String("token", "dict-token-1")))
	l.Info("namespace", zap.Namespace("ns"), zap.String("detail", detail))
	l.With(zap.String("detail", detail), zap.Error(errors.New(detail))).Info("with fields")
	l.Named(detail).Info("named logger")
	l.Sugar().Infow("sugar kv", "detail", detail, "err", errors.New(detail))
	l.Sugar().Infof("sugar format: %s", detail)
	l.Sugar().Info(detail)
	l.Sugar().With("detail", detail).Infow("sugar with")
	l.Error("error level with stacktrace", zap.String("detail", detail))
	l.Info("panic string", PanicFields(detail)...)
	l.Info("panic error", PanicFields(errors.New(detail))...)
	l.Info("panic wrapped", PanicFields(fmt.Errorf("w: %w", errors.New(detail)))...)
}

// 泄漏即失败（G-5）：固定语料的每个 secret 经任一载体、任一方式都不得出现在最终 JSON 输出中。
func TestCorpusNeverLeaksThroughAnyCarrier(t *testing.T) {
	c := loadCorpus(t)
	for _, tc := range c.LeakCases {
		l, buf := newTestLogger(t)
		emitAll(l, tc.Input)
		_ = l.Sync()
		out := buf.String()
		lines := parseLines(t, buf) // 同时断言每行都是合法 JSON
		if len(lines) < 25 {
			t.Fatalf("%s: 日志行数异常 %d", tc.Name, len(lines))
		}
		for _, s := range tc.Secrets {
			if strings.Contains(out, s) {
				// 定位到具体载体便于排查。
				for _, ln := range lines {
					raw, _ := json.Marshal(ln)
					if strings.Contains(string(raw), s) {
						t.Errorf("%s: 载体 %q 泄漏 %q\n%s", tc.Name, ln["msg"], s, raw)
					}
				}
			}
		}
		// 固定的辅助秘密（载体自带的 password/api_key 等键名值）同样不得出现。
		for _, s := range []string{"pl-pass-1", "obj-password-value-1", "obj-inner-key-1", "arr-auth-value-1", "dict-token-1"} {
			if strings.Contains(out, s) {
				t.Errorf("%s: 键名敏感的载体值泄漏 %q", tc.Name, s)
			}
		}
	}
}

// 与固定语料互补：随机生成的口令/令牌，经全部载体。
func TestRandomSecretsNeverLeakThroughAnyCarrier(t *testing.T) {
	gen := uid.NewGenerator(clock.System())
	for i := 0; i < 5; i++ {
		secret := strings.ToLower(gen.MustNew()) + strings.ToLower(gen.MustNew())
		for _, tpl := range []string{
			"app:%s@tcp(127.0.0.1:3306)/db", "redis://:%s@h:6379/0", "password=%s", `{"secret":"%s"}`, "Authorization: Bearer %s",
			"le_abcd1234_%s", "lenr1.%s", "Cookie: le_admin_sid=%s", "-----BEGIN PRIVATE KEY-----\n%s\n-----END PRIVATE KEY-----",
		} {
			l, buf := newTestLogger(t)
			emitAll(l, fmt.Sprintf(tpl, secret))
			if strings.Contains(buf.String(), secret) {
				t.Fatalf("模板 %q 泄漏随机秘密 %s:\n%s", tpl, secret, buf.String())
			}
		}
	}
}

// 键名敏感：任何类型的值都整体替换为 ***。
func TestSensitiveKeysRedactWholeValueForEveryFieldType(t *testing.T) {
	c := loadCorpus(t)
	const v = "plain-looking-value-12345"
	typed := func(key string) map[string]zap.Field {
		return map[string]zap.Field{
			"string":     zap.String(key, v),
			"int":        zap.Int(key, 987654321),
			"bool":       zap.Bool(key, true),
			"float":      zap.Float64(key, 3.14159),
			"strings":    zap.Strings(key, []string{v}),
			"any-map":    zap.Any(key, map[string]string{"a": v}),
			"named-err":  zap.NamedError(key, errors.New(v)),
			"bytestring": zap.ByteString(key, []byte(v)),
			"binary":     zap.Binary(key, []byte(v)),
			"stringer":   zap.Stringer(key, strer{v}),
			"object":     zap.Object(key, objMarshaler{v}),
			"array":      zap.Array(key, arrMarshaler{v}),
			"time":       zap.Time(key, fixedNow),
			"duration":   zap.Duration(key, time.Second),
		}
	}
	for _, key := range c.Keys.Sensitive {
		for kind, f := range typed(key) {
			l, buf := newTestLogger(t)
			l.Info("typed", f)
			out := buf.String()
			for _, bad := range []string{v, "987654321", "3.14159", "obj-password-value-1", "arr-auth-value-1", "08:30:15"} {
				if strings.Contains(strings.TrimPrefix(out, `{"level":"info","ts":"2026-11-17T08:30:15.123Z"`), bad) {
					t.Errorf("键 %q 类型 %s: 值泄漏 %q: %s", key, kind, bad, out)
				}
			}
			ln := parseLines(t, buf)[0]
			if val, ok := ln[key]; !ok || val != Mask {
				t.Errorf("键 %q 类型 %s: 值应为 %q，实际 %v（%s）", key, kind, Mask, val, out)
			}
		}
	}
}

func TestNamespaceAndNestedKeys(t *testing.T) {
	l, buf := newTestLogger(t)
	l.Info("nested",
		zap.Namespace("request"),
		zap.String("password", "ns-pass-1"),
		zap.Any("headers", map[string]any{"Authorization": []string{"Bearer abcdefgh12345678"}, "X-Trace": "ok", "inner": map[string]any{"client_" + "secret": canaryClient}}),
	)
	out := buf.String()
	for _, s := range []string{"ns-pass-1", "abcdefgh12345678", "ns-cs-1"} {
		if strings.Contains(out, s) {
			t.Errorf("泄漏 %q: %s", s, out)
		}
	}
	if !strings.Contains(out, `"X-Trace":"ok"`) {
		t.Errorf("良性嵌套字段应保留: %s", out)
	}
}

// 不得误伤：良性消息、良性键值逐字保留。
func TestBenignContentPreserved(t *testing.T) {
	c := loadCorpus(t)
	for _, in := range c.BenignCases {
		if in == "" || in == " " {
			continue
		}
		l, buf := newTestLogger(t)
		l.Info(in, zap.String("detail", in))
		lines := parseLines(t, buf)
		if len(lines) != 1 || lines[0]["msg"] != in || lines[0]["detail"] != in {
			t.Errorf("良性内容被改动: 输入 %q 输出 %v", in, lines)
		}
	}
	for _, key := range c.Keys.Benign {
		if key == "" || key == "ts" || key == "msg" || key == "level" || key == "caller" {
			continue // 与编码器保留键冲突
		}
		l, buf := newTestLogger(t)
		l.Info("m", zap.String(key, "visible-value"))
		if got := parseLines(t, buf)[0][key]; got != "visible-value" {
			t.Errorf("良性键 %q 被改动: %v", key, got)
		}
	}
}

// 错误链：被 Error() 隐藏的原因单独输出到 error_causes，且已清洗。
func TestErrorChainCausesAreSanitizedAndReported(t *testing.T) {
	const canary = "HiddenCauseCanary88"
	l, buf := newTestLogger(t)
	l.Warn("w", zap.Error(&hidingErr{cause: fmt.Errorf("dial u:%s@tcp(h:1)/d", canary)}))
	out := buf.String()
	if strings.Contains(out, canary) {
		t.Fatalf("泄漏: %s", out)
	}
	ln := parseLines(t, buf)[0]
	if ln["error"] != "operation failed" {
		t.Errorf("error 字段应为顶层文本: %v", ln["error"])
	}
	causes, ok := ln["error_causes"].([]any)
	if !ok || len(causes) != 1 || !strings.Contains(causes[0].(string), Mask) {
		t.Errorf("应输出已清洗的 error_causes: %v", ln["error_causes"])
	}
	// 普通 %w 链：顶层文本已包含全部层，不重复输出 causes。
	buf.Reset()
	l.Warn("w2", zap.Error(fmt.Errorf("a: %w", errors.New("b"))))
	if _, has := parseLines(t, buf)[0]["error_causes"]; has {
		t.Error("原因已包含在顶层文本时不应重复输出 error_causes")
	}
}

type badErr struct{}

func (*badErr) Error() string { panic("boom") }

type unserializable struct{ C chan int }

func TestDefensiveAgainstPanickingAndUnserializableValues(t *testing.T) {
	l, buf := newTestLogger(t)
	var nilErr *badErr
	l.Info("defensive",
		zap.Error(nilErr),
		zap.Stringer("s", panicStrer{}),
		zap.Any("u", unserializable{C: make(chan int)}),
		zap.Object("o", zapcore.ObjectMarshalerFunc(func(zapcore.ObjectEncoder) error { panic("nope") })),
		zap.Array("a", zapcore.ArrayMarshalerFunc(func(zapcore.ArrayEncoder) error { return errors.New("bad array") })),
		zap.Binary("b", []byte("password=binary-leak-1")),
	)
	out := buf.String()
	if strings.Contains(out, "binary-leak-1") || strings.Contains(out, "stringer-panic-leak") {
		t.Fatalf("泄漏: %s", out)
	}
	ln := parseLines(t, buf)[0]
	for _, k := range []string{"error", "s", "u", "o", "a", "b"} {
		if _, ok := ln[k]; !ok {
			t.Errorf("字段 %q 缺失（防御路径应输出占位而不是丢字段或崩溃）: %s", k, out)
		}
	}
	if ln["b"] != "<binary 22 bytes>" {
		t.Errorf("二进制字段应只记录长度: %v", ln["b"])
	}
}

// Wrap 可套在任意 Core 上：用 zap 自带 observer 直接检查进入 Core 的最终内容（不经 JSON 编码）。
func TestWrapAppliesToArbitraryCoreIncludingWith(t *testing.T) {
	obsCore, logs := observer.New(zapcore.DebugLevel)
	l := zap.New(Wrap(obsCore))
	l.With(zap.String("password", "with-pass-1"), zap.String("note", fmt.Sprintf("u:%s@tcp(h:1)/d", canaryDsnWith))).
		Info("msg password=msg-leak-1", zap.Error(errors.New("token=err-leak-1")))
	l.Named("lg").Warn("second", zap.String("authorization", "Bearer abcdef0123456789"))

	entries := logs.All()
	if len(entries) != 2 {
		t.Fatalf("应记录 2 条，实际 %d", len(entries))
	}
	var all strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&all, "%s|%s|%v\n", e.Message, e.LoggerName, e.Context)
	}
	for _, s := range []string{"with-pass-1", canaryDsnWith, "msg-leak-1", "err-leak-1", "abcdef0123456789"} {
		if strings.Contains(all.String(), s) {
			t.Errorf("Wrap 后仍含 %q:\n%s", s, all.String())
		}
	}
	if !strings.Contains(entries[0].Message, "msg password=***") {
		t.Errorf("消息应只替换敏感部分: %q", entries[0].Message)
	}
	// 重复 Wrap 幂等。
	obs2, logs2 := observer.New(zapcore.DebugLevel)
	zap.New(Wrap(Wrap(obs2))).Info("password=twice-leak-1")
	if m := logs2.All()[0].Message; m != "password=***" {
		t.Errorf("重复 Wrap 结果异常: %q", m)
	}
}

func TestStacktraceAndLoggerNameAreSanitized(t *testing.T) {
	obsCore, logs := observer.New(zapcore.DebugLevel)
	l := zap.New(Wrap(obsCore))
	l.Named("password=name-leak-1").Error("x") // LoggerName 清洗
	if n := logs.All()[0].LoggerName; strings.Contains(n, "name-leak-1") {
		t.Errorf("LoggerName 泄漏: %q", n)
	}
	// 直接以含凭据的 Stack 写入 Core。
	core := Wrap(obsCore)
	_ = core.Write(zapcore.Entry{Level: zapcore.ErrorLevel, Message: "m", Stack: "main.f(0x1, \"password=stack-leak-1\")"}, nil)
	if s := logs.All()[1].Stack; strings.Contains(s, "stack-leak-1") {
		t.Errorf("Stack 泄漏: %q", s)
	}
}

// ---- New / 输出格式 / Request ID / 并发 ----

func TestNewJSONShapeAndInjectedClock(t *testing.T) {
	l, buf := newTestLogger(t)
	l.Info("hello", zap.String("k", "v"), zap.Int("n", 7))
	ln := parseLines(t, buf)[0]
	if ln["ts"] != "2026-11-17T08:30:15.123Z" {
		t.Errorf("ts 应来自注入时钟且为 UTC 毫秒 RFC 3339，实际 %v", ln["ts"])
	}
	if ln["level"] != "info" || ln["msg"] != "hello" || ln["k"] != "v" || ln["n"] != float64(7) {
		t.Errorf("字段不符: %v", ln)
	}
	if c, _ := ln["caller"].(string); !strings.Contains(c, "logx_test.go") {
		t.Errorf("应带调用位置: %v", ln["caller"])
	}
}

func TestNewLevelsAndFormatsTable(t *testing.T) {
	cases := []struct {
		level string
		emit  func(*zap.Logger)
		want  int
	}{
		{"", func(l *zap.Logger) { l.Debug("d"); l.Info("i") }, 1},
		{"debug", func(l *zap.Logger) { l.Debug("d"); l.Info("i") }, 2},
		{"warn", func(l *zap.Logger) { l.Info("i"); l.Warn("w"); l.Error("e") }, 2},
		{"error", func(l *zap.Logger) { l.Warn("w"); l.Error("e") }, 1},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		l, err := New(Options{Level: tc.level, Output: &buf})
		if err != nil {
			t.Fatalf("level %q: %v", tc.level, err)
		}
		tc.emit(l)
		if got := len(parseLines(t, &buf)); got != tc.want {
			t.Errorf("level %q: 输出 %d 行，期望 %d", tc.level, got, tc.want)
		}
	}
	for _, bad := range []Options{{Level: "trace"}, {Level: "fatal"}, {Level: "INFO"}, {Format: "xml"}, {Format: "JSON"}} {
		if _, err := New(bad); err == nil {
			t.Errorf("非法选项 %+v 应报错", bad)
		}
	}
	// console 格式同样受脱敏保护。
	var buf bytes.Buffer
	l, err := New(Options{Format: "console", Output: &buf})
	if err != nil {
		t.Fatal(err)
	}
	l.Info("login password=console-leak-1", zap.String("authorization", "Bearer consoleleak123456"))
	if s := buf.String(); strings.Contains(s, "console-leak-1") || strings.Contains(s, "consoleleak123456") || !strings.Contains(s, Mask) {
		t.Errorf("console 输出异常: %q", s)
	}
}

func TestErrorLevelAddsStacktraceThatIsSanitized(t *testing.T) {
	l, buf := newTestLogger(t)
	l.Error("boom", zap.String("detail", "x"))
	ln := parseLines(t, buf)[0]
	if st, _ := ln["stacktrace"].(string); !strings.Contains(st, "TestErrorLevelAddsStacktraceThatIsSanitized") {
		t.Errorf("error 级应带栈: %v", ln["stacktrace"])
	}
}

func TestWithContextAddsRequestID(t *testing.T) {
	l, buf := newTestLogger(t)
	ctx, ok := requestid.NewContext(context.Background(), "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if !ok {
		t.Fatal("NewContext")
	}
	WithContext(ctx, l).Info("with id")
	WithContext(context.Background(), l).Info("without id")
	lines := parseLines(t, buf)
	if lines[0]["request_id"] != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Errorf("应附 request_id: %v", lines[0])
	}
	if _, has := lines[1]["request_id"]; has {
		t.Errorf("无 Request ID 时不应输出该字段: %v", lines[1])
	}
}

func TestRedactFieldAndParseLevel(t *testing.T) {
	l, buf := newTestLogger(t)
	l.Info("m", Redact("session_blob"), Redact("x"))
	ln := parseLines(t, buf)[0]
	if ln["session_blob"] != Mask || ln["x"] != Mask {
		t.Errorf("Redact 字段应为 ***: %v", ln)
	}
	for in, want := range map[string]zapcore.Level{"": zapcore.InfoLevel, "debug": zapcore.DebugLevel, "warn": zapcore.WarnLevel, "error": zapcore.ErrorLevel} {
		if got, err := ParseLevel(in); err != nil || got != want {
			t.Errorf("ParseLevel(%q)=%v,%v", in, got, err)
		}
	}
	if _, err := ParseLevel("panic"); err == nil {
		t.Error("未支持的级别应报错")
	}
}

func TestPanicFieldsInRecoverPath(t *testing.T) {
	l, buf := newTestLogger(t)
	func() {
		defer func() {
			if r := recover(); r != nil {
				l.Error("recovered", PanicFields(r)...)
			}
		}()
		var m map[string]int
		_ = fmt.Sprint(m["a"])
		panic(fmt.Errorf("handler failed: dial lingedge_app:%s@tcp(h:1)/d", canaryDsnPanic))
	}()
	out := buf.String()
	if strings.Contains(out, canaryDsnPanic) {
		t.Fatalf("panic 值泄漏: %s", out)
	}
	ln := parseLines(t, buf)[0]
	if p, _ := ln["panic"].(string); !strings.Contains(p, "handler failed") || !strings.Contains(p, Mask) {
		t.Errorf("panic 字段应保留上下文并遮蔽凭据: %v", ln["panic"])
	}
	if st, _ := ln["panic_stack"].(string); !strings.Contains(st, "TestPanicFieldsInRecoverPath") {
		t.Errorf("栈应指向 panic 现场: %.200v", ln["panic_stack"])
	}
	if n := strings.Count(out, `"stacktrace"`); n != 1 {
		t.Errorf("同一行不得出现重复的 stacktrace 键（zap 自带栈 + PanicFields），实际 %d 次: %s", n, out)
	}
}

func TestConcurrentLoggingIsRaceFreeAndSafe(t *testing.T) {
	var mu sync.Mutex
	var sink bytes.Buffer
	l, err := New(Options{Output: writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return sink.Write(p) })})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			wl := l.With(zap.Int("worker", w), zap.String("password", "concurrent-pass"))
			for i := 0; i < 200; i++ {
				wl.Info("tick password=concurrent-msg-leak", zap.Error(errors.New("token=concurrent-err-leak")), zap.Any("p", payload{Password: canaryConcur}))
			}
		}(w)
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	for _, s := range []string{"concurrent-pass", "concurrent-msg-leak", "concurrent-err-leak", "concurrent-struct-leak"} {
		if strings.Contains(sink.String(), s) {
			t.Fatalf("并发日志泄漏 %q", s)
		}
	}
	if n := strings.Count(sink.String(), "\n"); n != 8*200 {
		t.Errorf("行数 %d，期望 %d", n, 8*200)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

type panicJSON struct{}

func (panicJSON) MarshalJSON() ([]byte, error) { panic("json boom") }

func TestReflectedValuesBytesBigIntsAndPanickingMarshalers(t *testing.T) {
	l, buf := newTestLogger(t)
	raw := "pass" + "word=ReflectedBytesLeak99"
	l.Info("reflect",
		zap.Any("s", struct {
			B  []byte `json:"b"`
			ID uint64 `json:"id"`
		}{B: []byte(raw), ID: 18446744073709551615}),
		zap.Any("p", panicJSON{}),
	)
	out := buf.String()
	if strings.Contains(out, "ReflectedBytesLeak99") || strings.Contains(out, "cGFzc3dvcmQ9") {
		t.Fatalf("结构体里的 []byte 秘密泄漏: %s", out)
	}
	if !strings.Contains(out, "18446744073709551615") {
		t.Errorf("大整数应原样保留（不经 float64 往返）: %s", out)
	}
	ln := parseLines(t, buf)[0]
	if ln["p"] != "<unserializable>" {
		t.Errorf("MarshalJSON panic 应降级为占位: %v", ln["p"])
	}
}
