package security_test

import (
	"io"

	"go.uber.org/zap/zapcore"
)

// zapCoreForTest 构造一个不带脱敏的 JSON core，仅供「扫描自测」证明泄露会被发现。
func zapCoreForTest(w io.Writer) zapcore.Core {
	return zapcore.NewCore(zapcore.NewJSONEncoder(zapcore.EncoderConfig{MessageKey: "msg"}), zapcore.AddSync(w), zapcore.DebugLevel)
}
