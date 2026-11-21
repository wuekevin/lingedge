package cli

import (
	"encoding/json"
	"io"
	"time"

	"github.com/wuekevin/lingedge/pkg/redact"
)

// SchemaID 是 JSON envelope 的版本标识（tools-design §6）。
const SchemaID = "lingedge.cli/v1"

// Warning 是 envelope 的 warnings[] 项。
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrorItem 是 envelope 的 errors[] 项。code 为稳定小写点分串。
type ErrorItem struct {
	Code        string         `json:"code"`
	Message     string         `json:"message"`
	Step        int            `json:"step,omitempty"`
	Hint        string         `json:"hint,omitempty"`
	SystemState string         `json:"system_state,omitempty"`
	Details     map[string]any `json:"details,omitempty"`
}

// Envelope 是 `--json` 时 stdout 上唯一的输出文档（CF-5 冻结的契约）。字段顺序即序列化顺序。
type Envelope struct {
	Schema     string      `json:"schema"`
	OK         bool        `json:"ok"`
	Command    string      `json:"command"`
	RunID      string      `json:"run_id"`
	ExitCode   int         `json:"exit_code"`
	ExitName   string      `json:"exit_name"`
	StartedAt  string      `json:"started_at"`
	DurationMS int64       `json:"duration_ms"`
	DryRun     bool        `json:"dry_run"`
	Data       any         `json:"data"`
	Warnings   []Warning   `json:"warnings"`
	Errors     []ErrorItem `json:"errors"`
}

const timeLayout = "2006-01-02T15:04:05.000Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// sanitize 对 envelope 中所有可能携带用户输入的文本做脱敏：错误信息、提示、细节、警告与 data。
// data 经 JSON 往返后按通用结构清洗，保证任何命令（含将来的命令）都不会经 envelope 泄露口令/令牌。
func (e *Envelope) sanitize() {
	for i := range e.Warnings {
		e.Warnings[i].Message = redact.String(e.Warnings[i].Message)
	}
	for i := range e.Errors {
		it := &e.Errors[i]
		it.Message = redact.String(it.Message)
		it.Hint = redact.String(it.Hint)
		if it.Details != nil {
			if m, ok := redact.Value(it.Details).(map[string]any); ok {
				it.Details = m
			}
		}
	}
	if e.Data != nil {
		raw, err := json.Marshal(e.Data)
		if err != nil {
			return
		}
		var generic any
		dec := json.NewDecoder(bytesReader(raw))
		dec.UseNumber()
		if dec.Decode(&generic) == nil {
			e.Data = redact.Value(generic)
		}
	}
}

// write 把 envelope 作为单个 JSON 文档（一行，以换行结尾）写到 w。
func (e *Envelope) write(w io.Writer) error {
	if e.Warnings == nil {
		e.Warnings = []Warning{}
	}
	if e.Errors == nil {
		e.Errors = []ErrorItem{}
	}
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	e.sanitize()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}
