// Package repository 是 taskrt.Store 的 MySQL 实现（真实库，无 Mock）。
//
// 时间以数据库 NOW(3) 为准（ADR-019：避免节点间时钟差）；Worker 侧写回一律围栏；租户 Scope 下他租户的行不可见（0 行 = 404）。
// 查询文本全部是编译期常量，值经 ? 占位符传参（NFR-9）。
package repository
