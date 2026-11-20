// Package repository 是 Scheduler 运行时的持久层（Story 1.15）：job_locks 租约（LockStore）、sessions 保留期清理、分区表维护（maint 池）。
// 全部查询是编译期常量（db.Q）；时间以数据库 NOW(3) 为准。
package repository
