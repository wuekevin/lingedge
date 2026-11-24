package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"time"

	"go.yaml.in/yaml/v3"
)

// yamlLenient 把 YAML 解码进 v（不拒绝未知键）：用于「尽力读取」的场景（取 server.yaml 的监听地址），严格校验另有 cfgcheck。
func yamlLenient(b []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(v); err != nil && err != io.EOF {
		return err
	}
	return nil
}

// stateUpdatedAt 取 state.json 内部记录的 updated_at（时钟回拨判定用；解析失败则忽略）。
func stateUpdatedAt(b []byte) (time.Time, bool) {
	var s struct {
		UpdatedAt string `json:"updated_at"`
	}
	if json.Unmarshal(b, &s) != nil || s.UpdatedAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s.UpdatedAt)
	return t, err == nil
}

// readRegular 读取普通文件（上限 max 字节）；allowSymlink=false 时拒绝符号链接（Tools 管理的配置路径从不是符号链接）。
func readRegular(p string, max int64, allowSymlink bool) ([]byte, fs.FileInfo, error) {
	var st fs.FileInfo
	var err error
	if allowSymlink {
		st, err = os.Stat(p)
	} else {
		st, err = os.Lstat(p)
	}
	if err != nil {
		return nil, nil, err
	}
	if st.Mode()&fs.ModeSymlink != 0 {
		return nil, st, errSymlink
	}
	if !st.Mode().IsRegular() {
		return nil, st, errNotRegular
	}
	if st.Size() > max {
		return nil, st, errTooBig
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, st, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, st, err
	}
	if int64(len(b)) > max {
		return nil, st, errTooBig
	}
	return b, st, nil
}

type opsErr string

func (e opsErr) Error() string { return string(e) }

const (
	errSymlink    = opsErr("是符号链接（Tools 不跟随符号链接）")
	errNotRegular = opsErr("不是普通文件")
	errTooBig     = opsErr("文件过大（>1 MiB）")
)
