package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/security"
)

// 日志级别（--log-level）。
var logLevels = []string{"debug", "info", "warn", "error"}

func levelRank(l string) int {
	for i, x := range logLevels {
		if x == l {
			return i
		}
	}
	return -1
}

// fileLogger 写 JSON Lines 日志（tools-design §5：ts/level/run_id/step/msg/fields）。
// 所有 msg 与字段在落盘前经 pkg/redact（NFR-12、G-5）；参数中的敏感值先经 redactArgs 打码。
type fileLogger struct {
	mu    sync.Mutex
	f     *os.File
	min   int
	runID string
	now   func() time.Time
}

type logLine struct {
	TS     string         `json:"ts"`
	Level  string         `json:"level"`
	RunID  string         `json:"run_id"`
	Step   int            `json:"step,omitempty"`
	Msg    string         `json:"msg"`
	Fields map[string]any `json:"fields,omitempty"`
}

// openLogger 打开日志文件（O_APPEND，0640）。
// 显式 --log-file 无法打开是预检失败（退出码 3）；缺省路径无法打开（如普通用户运行只读命令）则静默禁用，返回 nil。
func openLogger(path string, explicit bool, level, runID string, now func() time.Time) (*fileLogger, *Error) {
	if path == "" {
		return nil, nil
	}
	open := func() (*os.File, error) {
		if explicit {
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				return nil, err
			}
		}
		return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	}
	f, err := open()
	if err != nil {
		if !explicit {
			return nil, nil
		}
		e := newErr(ExitPrecondition, "log.file_unwritable", "无法写入日志文件 "+path,
			"检查目录权限，或用 --log-file 指向可写位置（缺省 "+DefaultLogPath+" 需要 root）")
		e.Cause = err
		return nil, e
	}
	return &fileLogger{f: f, min: levelRank(level), runID: runID, now: now}, nil
}

func (l *fileLogger) close() {
	if l == nil {
		return
	}
	_ = l.f.Close()
}

// log 写一行；nil 接收者为空操作。
func (l *fileLogger) log(level string, step int, msg string, fields map[string]any) {
	if l == nil || levelRank(level) < l.min {
		return
	}
	line := logLine{TS: l.now().UTC().Format(time.RFC3339Nano), Level: level, RunID: l.runID, Step: step, Msg: security.RedactString(msg)}
	if len(fields) > 0 {
		if m, ok := security.RedactValue(fields).(map[string]any); ok {
			line.Fields = m
		}
	}
	b, err := json.Marshal(line)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// 日志写失败不应让命令失败（日志是辅助通道）。
	_, _ = l.f.Write(append(b, '\n'))
}

// redactArgs 返回可安全记录的 argv 副本（AC6）。规则实现于 internal/tools/security.RedactArgs（Story 1.26 起，
// 其内部复用 pkg/redact，全 Tools 只有这一处命令行脱敏规则）。
func redactArgs(args []string) []string { return security.RedactArgs(args) }

// argvString 用于提示文本（如 sudo 提示）：元素含空白/元字符时加单引号，仅用于显示，从不交给 shell 执行。
func argvString(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = shellQuoteForDisplay(a)
	}
	return strings.Join(parts, " ")
}

func shellQuoteForDisplay(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		isAlnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		return !isAlnum && !strings.ContainsRune("-_./:=@,+%", r)
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
