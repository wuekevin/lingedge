// Package updater 是升级相关的、不依赖 installer 事务机制的部分（Story 1.43，tools-design §17）：
//   - Check：由已验签的清单与本机 state.json 算出「可升级组件 + 迁移预告」（`update check`、`version --check`、TUI 同源）；
//   - SwapBinary：Tools 自身的原子替换（`update self`：先写 lingedge.new 并自检，再 rename 覆盖，保留 .prev）。
//
// 升级事务本身（13 步、journal 撤销、备份/迁移/切换/健康检查/回滚）与 22 步安装共用取包、journal、撤销与服务控制，落在 installer 包
// （与 uninstall 同处；tools-design §2 的 updater→installer 依赖方向因此反转为 installer 使用本包与 rollback，见 tools-design §17.7）。
package updater
