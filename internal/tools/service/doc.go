// Package service 是 LingEdge Tools 的「服务落地」层（Story 1.32，tools-design §15、§8、§21，ADR-024、ADR-020 §2）：
//
//   - 渲染 systemd 单元：模板经 go:embed 内置，所有变量先过 security.ValidateUnitValue / ValidateUnitPath，不接受自由文本（Render）；
//   - 运行用户：仅当不存在才创建（经 safeexec 的 id/getent/useradd），并记录「是否由本事务创建」（Provisioner.EnsureUser）；
//   - 目录与文件权限：按 §8 与 ADR-020 §2 总表创建，创建即最终权限（Ensurer、FileWriter）；
//   - systemctl 封装：全部参数数组、仅认识固定的单元名，经 safeexec（Systemctl）；
//   - 单元安装：同名单元已存在则不覆盖，写 `.new` 并警告，内容一致视为幂等完成（Installer，FR-6）；
//   - LoadCredential 最低版本与降级（ADR-027）：systemctl --version 解析、版本判定、降级渲染变体、doctor 检查函数；
//   - 撤销：所有会改系统的操作都产出 registry.UndoAction（含 Created），Undoer 能执行它们，供 1.33 编排与 recover 使用。
//
// 本包不编排安装步骤（那是 1.33），不创建进程以外的任何入口：外部程序只能经 safeexec（Runner）。
// Linux 专属判定在 platform_linux.go，其他平台有明确降级（见 platform_other.go）。
package service
