package logx

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/requestid"
	"github.com/wuekevin/lingedge/pkg/redact"
)

// Options 是 New 的参数。零值可用：info 级、JSON、写 stderr、系统时钟。
type Options struct {
	Level  string      // debug|info|warn|error（默认 info）
	Format string      // json|console（默认 json；生产与 CI 采集一律 json）
	Output io.Writer   // 默认 os.Stderr
	Clock  clock.Clock // 默认 clock.System()；测试注入以得到确定的时间戳
}

// 受支持的取值（config 包复用同一份，避免两处漂移）。
var (
	Levels  = []string{"debug", "info", "warn", "error"}
	Formats = []string{"json", "console"}
)

// ParseLevel 解析日志级别（小写，与配置校验同一口径）；空串为 info。
func ParseLevel(s string) (zapcore.Level, error) {
	switch s {
	case "", "info":
		return zapcore.InfoLevel, nil
	case "debug":
		return zapcore.DebugLevel, nil
	case "warn":
		return zapcore.WarnLevel, nil
	case "error":
		return zapcore.ErrorLevel, nil
	}
	return 0, fmt.Errorf("logx: 不支持的日志级别（可选 %s）", strings.Join(Levels, "|"))
}

// New 构造带脱敏过滤的 Logger：JSON（或 console）、UTC 毫秒 RFC 3339 时间戳、小写级别、error 级附栈。
func New(o Options) (*zap.Logger, error) {
	lvl, err := ParseLevel(o.Level)
	if err != nil {
		return nil, err
	}
	out := o.Output
	if out == nil {
		out = os.Stderr
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.System()
	}

	cfg := zapcore.EncoderConfig{
		TimeKey: "ts", LevelKey: "level", NameKey: "logger", CallerKey: "caller", MessageKey: "msg", StacktraceKey: "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeTime:     utcMillisTime,
		EncodeDuration: zapcore.MillisDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}
	var enc zapcore.Encoder
	switch o.Format {
	case "", "json":
		enc = zapcore.NewJSONEncoder(cfg)
	case "console":
		enc = zapcore.NewConsoleEncoder(cfg)
	default:
		return nil, fmt.Errorf("logx: 不支持的日志格式（可选 %s）", strings.Join(Formats, "|"))
	}

	core := Wrap(zapcore.NewCore(enc, zapcore.Lock(zapcore.AddSync(out)), lvl))
	return zap.New(core, zap.WithClock(zapClock{clk}), zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel)), nil
}

// utcMillisTime 把时间编码为 2026-11-17T08:00:00.000Z（UTC，毫秒）。
func utcMillisTime(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
	enc.AppendString(t.UTC().Format("2006-01-02T15:04:05.000Z"))
}

// zapClock 把 clock.Clock 适配为 zapcore.Clock。
type zapClock struct{ c clock.Clock }

func (z zapClock) Now() time.Time                         { return z.c.Now() }
func (z zapClock) NewTicker(d time.Duration) *time.Ticker { return time.NewTicker(d) }

// Redact 返回显式标记为秘密的字段：值永不输出，只输出 ***（ADR-011 §7 的 Redact 字段类型）。
func Redact(key string) zap.Field { return zap.String(key, Mask) }

// PanicFields 把 recover() 得到的值转为日志字段：panic 值与调用栈都已脱敏。须在 defer 的 recover 处理函数内调用，
// 以便栈指向 panic 现场。不要直接把 recover() 的值传给 zap.Any / 其他输出路径。
func PanicFields(recovered any) []zap.Field {
	return []zap.Field{zap.String("panic", redact.Panic(recovered)), zap.String("panic_stack", redact.Stack())}
}

// WithContext 返回附带 request_id 字段的 Logger（上下文没有 Request ID 时原样返回）。
func WithContext(ctx context.Context, l *zap.Logger) *zap.Logger {
	if id, ok := requestid.FromContext(ctx); ok {
		return l.With(zap.String("request_id", id))
	}
	return l
}
