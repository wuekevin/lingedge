// Package logx 把脱敏引擎（pkg/redact）接入 Zap（Story 1.4 AC2、AC4；ADR-011 §7、NFR-12、G-5）。
//
// 保证：经本包构造或包装的 Logger，任何一条日志在写出前都会被过滤——
//   - 消息、logger 名、调用栈：按值格式清洗；
//   - 字段键名敏感（password|secret|token|private_key|authorization|cookie…）：值整体替换为 ***，无论字段类型；
//   - 字符串/字节串/Stringer 字段：按值格式清洗；
//   - error 字段：错误链上每一层的消息都被清洗（含 errors.Join 分支与被 Error() 隐藏的被包装原因）；
//   - 结构体/map/slice（Reflect）、ObjectMarshaler、ArrayMarshaler：递归按键名与值格式清洗；
//   - panic 值：用 PanicFields 记录（recover 中间件使用），值与栈同样清洗。
//
// 与 With 预置字段、Named、Sugar 的组合同样受保护（过滤发生在 Core 层，所有路径必经）。
package logx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/wuekevin/lingedge/pkg/redact"
)

// Mask 是被替换内容的占位文本。
const Mask = redact.Mask

// Wrap 返回对 inner 施加脱敏过滤的 Core。重复 Wrap 是幂等安全的（清洗本身幂等）。
func Wrap(inner zapcore.Core) zapcore.Core { return &redactCore{inner: inner} }

type redactCore struct{ inner zapcore.Core }

func (c *redactCore) Enabled(l zapcore.Level) bool { return c.inner.Enabled(l) }

func (c *redactCore) With(fields []zapcore.Field) zapcore.Core {
	return &redactCore{inner: c.inner.With(sanitizeFields(fields))}
}

func (c *redactCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c *redactCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	ent.Message = redact.String(ent.Message)
	ent.LoggerName = redact.String(ent.LoggerName)
	ent.Stack = redact.String(ent.Stack)
	return c.inner.Write(ent, sanitizeFields(fields))
}

func (c *redactCore) Sync() error { return c.inner.Sync() }

func sanitizeFields(in []zapcore.Field) []zapcore.Field {
	if len(in) == 0 {
		return in
	}
	out := make([]zapcore.Field, 0, len(in)+2)
	for _, f := range in {
		out = append(out, sanitizeField(f)...)
	}
	return out
}

// 键名敏感时整体替换；其余按类型清洗。返回切片是因为 error 字段可能附带「原因链」字段。
func sanitizeField(f zapcore.Field) []zapcore.Field {
	switch f.Type {
	case zapcore.SkipType, zapcore.NamespaceType:
		return []zapcore.Field{f}
	}
	if redact.Key(f.Key) {
		return []zapcore.Field{zap.String(f.Key, Mask)}
	}
	switch f.Type {
	case zapcore.StringType:
		f.String = redact.String(f.String)
		return []zapcore.Field{f}
	case zapcore.ByteStringType:
		if b, ok := f.Interface.([]byte); ok {
			f.Interface = redact.Bytes(b)
		}
		return []zapcore.Field{f}
	case zapcore.BinaryType:
		// 二进制载荷无法判定内容：只记录长度。
		n := 0
		if b, ok := f.Interface.([]byte); ok {
			n = len(b)
		}
		return []zapcore.Field{zap.String(f.Key, fmt.Sprintf("<binary %d bytes>", n))}
	case zapcore.StringerType:
		return []zapcore.Field{zap.String(f.Key, stringerText(f.Interface))}
	case zapcore.ErrorType:
		return errorFields(f)
	case zapcore.ReflectType:
		return []zapcore.Field{reflectField(f.Key, f.Interface)}
	case zapcore.ObjectMarshalerType:
		m, ok := f.Interface.(zapcore.ObjectMarshaler)
		if !ok {
			return []zapcore.Field{zap.String(f.Key, "<unserializable>")}
		}
		enc := zapcore.NewMapObjectEncoder()
		if err := safeMarshalObject(m, enc); err != nil {
			return []zapcore.Field{zap.String(f.Key, "<unserializable>")}
		}
		return []zapcore.Field{zap.Any(f.Key, redact.Value(enc.Fields))}
	case zapcore.InlineMarshalerType:
		m, ok := f.Interface.(zapcore.ObjectMarshaler)
		if !ok {
			return []zapcore.Field{zap.String(f.Key, "<unserializable>")}
		}
		enc := zapcore.NewMapObjectEncoder()
		if err := safeMarshalObject(m, enc); err != nil {
			return []zapcore.Field{zap.String(f.Key, "<unserializable>")}
		}
		clean, _ := redact.Value(enc.Fields).(map[string]any)
		keys := make([]string, 0, len(clean))
		for k := range clean {
			keys = append(keys, k)
		}
		sort.Strings(keys) // 输出顺序确定
		out := make([]zapcore.Field, 0, len(clean))
		for _, k := range keys {
			out = append(out, zap.Any(k, clean[k]))
		}
		return out
	case zapcore.ArrayMarshalerType:
		m, ok := f.Interface.(zapcore.ArrayMarshaler)
		if !ok {
			return []zapcore.Field{zap.String(f.Key, "<unserializable>")}
		}
		enc := zapcore.NewMapObjectEncoder()
		if err := safeMarshalArray(enc, f.Key, m); err != nil {
			return []zapcore.Field{zap.String(f.Key, "<unserializable>")}
		}
		return []zapcore.Field{zap.Any(f.Key, redact.Value(enc.Fields[f.Key]))}
	default:
		// 布尔/数值/时间/时长等标量：不含自由文本，原样输出。
		return []zapcore.Field{f}
	}
}

func safeMarshalObject(m zapcore.ObjectMarshaler, enc zapcore.ObjectEncoder) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("marshal panic")
		}
	}()
	return m.MarshalLogObject(enc)
}

func safeMarshalArray(enc zapcore.ObjectEncoder, key string, m zapcore.ArrayMarshaler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("marshal panic")
		}
	}()
	return enc.AddArray(key, m)
}

func stringerText(v any) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = "<panic in String()>"
		}
	}()
	s, ok := v.(fmt.Stringer)
	if !ok {
		return "<unserializable>"
	}
	return redact.String(s.String())
}

// errorFields：<key> 为整条错误文本（已清洗）；错误链上存在 Error() 未体现的被包装原因时，另附 <key>_causes。
func errorFields(f zapcore.Field) []zapcore.Field {
	err, ok := f.Interface.(error)
	if !ok || err == nil {
		return []zapcore.Field{zap.String(f.Key, "<nil>")}
	}
	top := safeErrorText(err)
	out := []zapcore.Field{zap.String(f.Key, top)}
	var causes []string
	for _, m := range redact.Chain(err)[1:] {
		if m != "" && !strings.Contains(top, m) {
			causes = append(causes, m)
		}
	}
	if len(causes) > 0 {
		out = append(out, zap.Strings(f.Key+"_causes", causes))
	}
	return out
}

func safeErrorText(err error) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = "<panic in Error()>"
		}
	}()
	return redact.String(err.Error())
}

// reflectField 经 JSON 往返得到通用结构后按键名与值格式清洗；无法序列化时只输出占位。
func reflectField(key string, v any) (f zapcore.Field) {
	defer func() { // 自定义 MarshalJSON 可能 panic：日志路径不得把它放出去
		if r := recover(); r != nil {
			f = zap.String(key, "<unserializable>")
		}
	}()
	b, err := json.Marshal(v)
	if err != nil {
		return zap.String(key, "<unserializable>")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // 大整数（ID、字节计数）不经 float64 往返，避免精度丢失
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return zap.String(key, "<unserializable>")
	}
	return zap.Any(key, redact.Value(generic))
}
