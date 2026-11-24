// Package rollback 是升级的「迁移分级决策表」与回滚路径的执行（Story 1.43，tools-design §17.2–§17.4）。
//
// Decide 把升级所跨越的迁移级别集合映射为备份/确认/回滚路径/对用户的承诺（纯函数，update check、--dry-run、TUI 确认页同源）；
// Executor 执行回滚路径的各个动作——原子切换 current、migrate down、恢复升级前备份（含启动旧版本 Server 之前的 lineage bump）、保留策略清理——
// 它被升级失败的自动回滚（installer 的 journal 撤销）、手动 `lingedge rollback` 与 `recover --rollback` 共用，所有动作都是幂等的。
package rollback
