package cli

import (
	"context"

	"github.com/wuekevin/lingedge/internal/tools/registry"
)

// ComponentInfo 是 `list` 的 data.components[] 项（tools-design §6）。
// 其数据来自 registry 包（Story 1.29）读取的 state.json；输出形状由本包固定（JSON Schema 见 schema/data.list.schema.json）。
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

// StateSource 提供本机已安装组件。缺省实现是读取 state.json 的 FileState（经 registry）；测试可注入自己的实现来验证渲染。
type StateSource interface {
	Components(ctx context.Context) ([]ComponentInfo, error)
}

// FileState 经 registry 读取 state.json（Story 1.29 替换了 1.25 的「state.reader_unavailable」占位）。
//
// 文件不存在 → 空列表（AC5：version/list/status 对无 state.json 返回空而非报错）。
// 文件存在 → 严格解析与校验：损坏/未知 schema 版本/权限不安全都如实报错（见 RegistryError 的映射），绝不把已有但读不懂的状态当作空来展示。
// 读取不加锁：registry 的原子写保证读者看到的总是旧版或新版的完整文件，所以普通用户也能安全运行 list/status。
type FileState struct{ Path string }

// Components 实现 StateSource。
func (f FileState) Components(context.Context) ([]ComponentInfo, error) {
	reg := registry.New(registry.LayoutForStatePath(f.Path), registry.Options{})
	st, _, err := reg.LoadState()
	if err != nil {
		return nil, RegistryError(err)
	}
	out := make([]ComponentInfo, 0, len(st.Components))
	for _, name := range st.Names() {
		c := st.Components[name]
		ports := c.Ports
		if ports == nil {
			ports = []int{}
		}
		out = append(out, ComponentInfo{
			Name: name, Version: c.Version, State: c.State, Unit: c.Unit, RunUser: c.RunUser, Ports: ports,
			ReleaseDir: c.ReleaseDir, ConfigDir: c.ConfigDir, DataDir: c.DataDir, InstalledAt: c.InstalledAt, PrevVersion: c.PrevVersion,
		})
	}
	return out, nil
}
