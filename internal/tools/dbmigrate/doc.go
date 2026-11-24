// Package dbmigrate 是 Tools 对 `lingedge-server migrate` 的调用封装（升级编排的步骤 7 `migrate plan` 与步骤 9 `migrate up`，以及回滚路径的 `migrate down`，
// tools-design §17.2/§17.3，Story 1.43）。
//
// 迁移凭据（AC9，ADR-026 §4）：口令只经环境变量传给 `migrate` 的**直接子进程**（safeexec 子进程不继承父进程环境，只带显式给出的变量），
// 连接目标只经命令行（host/port/socket/database/user/tls）；Tools 不写文件、不经 systemd、不进 journal/state.json/日志。
// 凭据本身的来源优先级（环境变量 ＞ 口令文件 ＞ migrate.yaml ＞ 交互输入）由 backup.ResolveDB 实现，本包只消费已解析的 backup.DBConn。
package dbmigrate
