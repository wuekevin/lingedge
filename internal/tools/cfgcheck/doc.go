// Package cfgcheck 校验 Tools 管理的配置文件并给出「键 + 行列」的问题位置（Story 1.42，`config validate`/`doctor cfg.validate`/`config edit`）。
//
// 校验对象与来源：
//   - tools.yaml：本包的严格 schema（反射 installer.ToolsConfig 的 yaml 标签：未知键、重复键、类型、取值，与 install 读取它时的规则是同一份）；
//   - server.yaml：权威校验是 `lingedge-server config check`（与 Server 启动同一个 config.Parse；Tools 不 import Server 的 internal 包）。
//     Server 二进制不可用时（尚未安装、无法执行）退回本包的结构校验（键集合、类型、口令不得写在文件里、监听地址形态），并明确标注「未做完整校验」；
//   - node/dns/reporter 的 yaml：对应组件尚未交付 schema（Epic 3），只做语法校验（语法、重复键、单文档、顶层为映射），并明确标注；
//   - 静态前端的 Nginx 片段：installer.LintNginxSnippet / LintSecurityHeaders（只读、不执行 nginx）。
//
// 所有问题文本只含键路径与规则说明，永不回显配置取值（避免把写错位置的口令带进输出与日志）。
package cfgcheck
