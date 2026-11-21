package cli

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/wuekevin/lingedge/pkg/redact"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

func redactString(s string) string { return redact.String(s) }

func join(s []string, sep string) string { return strings.Join(s, sep) }

// wf/wln 写人类输出；写 stdout/stderr 失败时无处可报，退出码语义不变（与 pkg/version.CLI 同一约定）。
func wf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
func wln(w io.Writer, a ...any)               { _, _ = fmt.Fprintln(w, a...) }
