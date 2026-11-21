package cli

import (
	"context"
	"errors"
	"io/fs"
	"os"
)

// ComponentInfo 是 `list` 的 data.components[] 项（tools-design §6）。
// 其数据由 registry 包（Story 1.29）从 state.json 提供；本 Story 只固定输出形状。
type ComponentInfo struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	State       string `json:"state"`
	Unit        string `json:"unit"`
	RunUser     string `json:"run_user"`
	Ports       []int  `json:"ports"`
	ReleaseDir  string `json:"release_dir"`
	ConfigDir   string `json:"config_dir"`
	DataDir     string `json:"data_dir"`
	InstalledAt string `json:"installed_at"`
	PrevVersion string `json:"prev_version,omitempty"`
}

// StateSource 提供本机已安装组件。registry（Story 1.29）实现后在 cmd/lingedge 装配注入。
type StateSource interface {
	Components(ctx context.Context) ([]ComponentInfo, error)
}

// FileState 读取 state.json。
//
// 文件不存在 → 空列表（AC5：version/list/status 对无 state.json 返回空而非报错）。
// 文件存在 → state.json 的格式与读写由 registry（Story 1.29）定义，本 Story 不猜测其格式：
// 如实返回「读取器尚未交付」错误（退出码 1），而不是把已有状态当作空来展示（那会是假成功）。
type FileState struct{ Path string }

// Components 实现 StateSource。
func (f FileState) Components(context.Context) ([]ComponentInfo, error) {
	_, err := os.Stat(f.Path)
	switch {
	case err == nil:
		return nil, &Error{
			Exit: ExitGeneral, Code: "state.reader_unavailable",
			Message: "发现 " + f.Path + "，但 state.json 的读取器（registry 包）尚未交付，无法给出真实的已装组件列表",
			Hint:    "state.json 读写随 Story 1.29 交付；在此之前请勿依赖 list/status/version 的组件信息",
		}
	case errors.Is(err, fs.ErrNotExist):
		return []ComponentInfo{}, nil
	default:
		return nil, &Error{
			Exit: ExitPrecondition, Code: "state.unreadable",
			Message: "无法读取 " + f.Path, Hint: "检查该文件及其父目录的权限（state.json 应为 0644，普通用户可读）", Cause: err,
		}
	}
}
