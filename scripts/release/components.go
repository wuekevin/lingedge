package main

import "github.com/wuekevin/lingedge/internal/tools/manifest"

// 发行组件目录（Story 1.34；清单语义见 tools-design §9.2、§2 组件表）。
//
// 本表是「哪些组件在本发行里真的有包」的唯一事实来源，状态必须如实：
//   - available：本仓库当前能构建出真实包（server、tools）；
//   - unreleased：对应交付物尚不存在（node/dns/reporter 的 cmd 目前只有 --version 脚手架，OpenResty 发行包属 Epic 3 的 OpenResty Story）；
//     静态前端（admin-web/console/docs/status/website）的交付物是前端生产构建目录，经 -frontend-dist <组件>=<目录> 提供时才变为 available（Story 1.40，见 frontend.go），
//     没有提供的保持 unreleased（docs/status 目前没有任何构建产物源，console 是基座探针，website 属 Epic 2——不发布空壳包）。
//     清单里如实写 "unreleased"，Tools 以退出码 7 回答，**不得为了让清单「好看」而发布空壳包**；
//   - builtin：scheduler/worker 内置于 server。
//
// 某组件的交付物落地后，只需在这里把状态改为 available 并补上 Binaries/Files（或经 -openresty-* 标志提供 OpenResty 目录树），流水线其余部分不变。
type compDef struct {
	Name        string
	Status      string
	Description string
	Internal    bool
	BuiltinIn   string

	Variant  string   // 缺省 default
	Binaries []string // cmd/<name> → payload/bin/<name>
	Files    []fileMap

	Requires            *manifest.Requires
	Ports               []manifest.Port
	Migrations          bool // 迁移预告取自 lingedge-server migrate plan
	ConfigSchemaVersion int
	Platforms           []string // 缺省 -platforms
}

type fileMap struct{ Src, Dst string } // Src 相对源仓库根；Dst 相对 payload/

// 数值取自 tools-design §9.2 示例（发行方填写，Tools 不内置）。[ASSUMPTION]：最低内存 1024MB、磁盘 2048MB、
// MySQL ">=8.0.16 <9"、Redis ">=8" 与基线一致；正式发行前由发行负责人复核。
var catalog = []compDef{
	{
		Name: "server", Status: manifest.CompAvailable, Description: "LingEdge 中央主控",
		Binaries: []string{"lingedge-server"},
		Files:    []fileMap{{Src: "configs/server.example.yaml", Dst: "share/config/server.example.yaml"}},
		Requires: &manifest.Requires{
			System:   &manifest.SystemReq{OS: []string{"linux"}, Arch: []string{"amd64", "arm64"}, MinMemoryMB: 1024, MinDiskMB: 2048},
			External: map[string]string{"mysql": ">=8.0.16 <9", "redis": ">=8"},
		},
		Ports: []manifest.Port{
			{Proto: "tcp", Port: 8080, Bind: "127.0.0.1", Purpose: "rest"},
			{Proto: "tcp", Port: 9443, Bind: "0.0.0.0", Purpose: "grpc-mtls"},
			{Proto: "tcp", Port: 9444, Bind: "0.0.0.0", Purpose: "grpc-enroll", Conditional: true},
			{Proto: "tcp", Port: 9180, Bind: "127.0.0.1", Purpose: "metrics"},
		},
		Migrations: true, ConfigSchemaVersion: 1,
	},
	{
		Name: "tools", Status: manifest.CompAvailable, Description: "LingEdge Tools（lingedge 命令行）",
		Binaries: []string{"lingedge"},
		Requires: &manifest.Requires{System: &manifest.SystemReq{OS: []string{"linux"}, Arch: []string{"amd64", "arm64"}}},
	},
	{Name: "node", Status: manifest.CompUnreleased, Description: "LingEdge 边缘节点（待 Epic 3：需要 OpenResty 与 Agent 实现）"},
	{Name: "openresty", Status: manifest.CompUnreleased, Internal: true, Description: "OpenResty 发行组件（待 Epic 3；需来源方案 UD-4 与真实目录树）"},
	{Name: "dns", Status: manifest.CompUnreleased},
	{Name: "reporter", Status: manifest.CompUnreleased},
	{Name: "admin-web", Status: manifest.CompUnreleased},
	{Name: "console", Status: manifest.CompUnreleased},
	{Name: "website", Status: manifest.CompUnreleased},
	{Name: "docs", Status: manifest.CompUnreleased},
	{Name: "status", Status: manifest.CompUnreleased},
	{Name: "scheduler", Status: manifest.CompBuiltin, BuiltinIn: "server"},
	{Name: "worker", Status: manifest.CompBuiltin, BuiltinIn: "server"},
}

func (c compDef) variant() string {
	if c.Variant == "" {
		return "default"
	}
	return c.Variant
}
