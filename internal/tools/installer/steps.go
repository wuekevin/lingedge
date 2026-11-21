package installer

// Phase 是 22 步的阶段（tools-design §12.1/§12.2）。
type Phase string

// Phase 取值。
const (
	PhasePreflight   Phase = "preflight"   // 1–9：只读
	PhaseFetch       Phase = "fetch"       // 10–13：写 staging
	PhaseMaterialize Phase = "materialize" // 14–18：写系统
	PhaseActivate    Phase = "activate"    // 19–20
	PhaseCommit      Phase = "commit"      // 21–22
)

// 步骤编号（基线 §4.5 / tools-design §12.1；journal 的 step 字段即这些数）。
const (
	StepOSDetect       = 1
	StepArch           = 2
	StepMemDisk        = 3
	StepPorts          = 4
	StepBaota          = 5
	StepWebServer      = 6
	StepDatabases      = 7
	StepDependencies   = 8
	StepVersion        = 9
	StepFetchManifest  = 10
	StepVerifyManifest = 11
	StepDownload       = 12
	StepVerifyPackage  = 13
	StepExtract        = 14
	StepCreateUser     = 15
	StepCreateDirs     = 16
	StepInstallUnits   = 17
	StepInitConfig     = 18
	StepStart          = 19
	StepHealth         = 20
	StepSaveState      = 21
	StepAccessInfo     = 22
	TotalSteps         = 22
)

// StepInfo 描述一步。
type StepInfo struct {
	N     int    `json:"n"`
	Name  string `json:"name"`
	Phase Phase  `json:"phase"`
	Title string `json:"title"`
	// SideEffects 为 false 的步骤（预检、第 13/22 步）的 undo 为空。
	SideEffects bool `json:"side_effects"`
}

// Steps 是 22 步的权威表（索引 = N-1）。
var Steps = [TotalSteps]StepInfo{
	{1, "os_detect", PhasePreflight, "OS 检测", false},
	{2, "cpu_arch", PhasePreflight, "CPU 架构", false},
	{3, "memory_disk", PhasePreflight, "内存/磁盘", false},
	{4, "port_conflict", PhasePreflight, "端口冲突", false},
	{5, "baota_env", PhasePreflight, "宝塔环境", false},
	{6, "web_server", PhasePreflight, "Nginx/OpenResty", false},
	{7, "databases", PhasePreflight, "MySQL/Redis", false},
	{8, "dependencies", PhasePreflight, "组件依赖分析", false},
	{9, "version_selection", PhasePreflight, "版本选择", false},
	{10, "fetch_manifest", PhaseFetch, "下载签名版本清单", true},
	{11, "verify_manifest", PhaseFetch, "验证清单签名", true},
	{12, "download_package", PhaseFetch, "下载组件包", true},
	{13, "verify_package", PhaseFetch, "校验 SHA256 与签名", true},
	{14, "extract_release", PhaseMaterialize, "安全解压", true},
	{15, "create_user", PhaseMaterialize, "创建运行用户", true},
	{16, "create_dirs", PhaseMaterialize, "创建配置/数据/日志目录", true},
	{17, "install_units", PhaseMaterialize, "安装 systemd Unit", true},
	{18, "init_config", PhaseMaterialize, "初始化配置", true},
	{19, "start_service", PhaseActivate, "启动服务", true},
	{20, "health_check", PhaseActivate, "健康检查", false},
	{21, "save_state", PhaseCommit, "保存安装状态（提交点）", true},
	{22, "access_info", PhaseCommit, "输出访问/管理信息", false},
}

// StepName 返回步骤的稳定名称；越界返回空串。
func StepName(n int) string {
	if n < 1 || n > TotalSteps {
		return ""
	}
	return Steps[n-1].Name
}

// PhaseOf 返回步骤所属阶段；越界返回空串。
func PhaseOf(n int) Phase {
	if n < 1 || n > TotalSteps {
		return ""
	}
	return Steps[n-1].Phase
}

// PlanStepMap 把 resolver.Step.Kind（高层步骤）映射到 §12.1 的 22 步（Story 1.30 留给本 Story 的映射）：
// download 横跨取清单/验签/下载/校验；extract 与 activate（install 时创建 current 链接）同属第 14 步；
// prepare 是用户（15）与目录（16）；units 17；configure 18；start 19；health 20；record 21。
var PlanStepMap = map[string][]int{
	"download":  {StepFetchManifest, StepVerifyManifest, StepDownload, StepVerifyPackage},
	"extract":   {StepExtract},
	"prepare":   {StepCreateUser, StepCreateDirs},
	"configure": {StepInitConfig},
	"units":     {StepInstallUnits},
	"activate":  {StepExtract},
	"start":     {StepStart},
	"health":    {StepHealth},
	"record":    {StepSaveState},
}
