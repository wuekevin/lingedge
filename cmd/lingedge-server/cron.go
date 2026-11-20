package main

import (
	"context"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/schedrt"
	"github.com/wuekevin/lingedge/internal/platform/server"
)

// registerCron 是后续 Story 登记自己的周期任务的唯一装配点（Story 1.15）。
//
// Scheduler 角色自带四个内部周期作业（tasks.reap / idempotency.cleanup / sessions.cleanup / partition.maintain，不属 TaskType，AA-8），
// 它们不在这里登记。各业务 Story 完成时在此追加自己的周期任务，并同时在 tasks.go 登记对应 TaskType 的处理函数，例如：
//
//	every, _ := schedrt.Every(time.Minute)
//	err := reg.Task("heartbeat.sweep", every, schedrt.TaskSpec{Type: taskrt.TypeHeartbeatSweep})
//
// 任务型作业以确定性幂等键 cron:<作业名>:<窗口起点> 入队：多个 Scheduler 因租约重叠同时触发也只会产生一条任务（uk_tasks_idem）。
// 当前没有任何业务周期任务（对应的 TaskType 处理函数尚未实现），因此这里不登记，也不伪造。
func registerCron(*schedrt.Registry, server.Deps) error { return nil }

// openMaint 以 lingedge_maint 账号打开 maint 连接池（仅 partition.maintain 使用）。调用点位于 cmd/lingedge-server，属门禁 [db-pool-maint] 的白名单。
func openMaint(ctx context.Context, d config.Database) (*db.MaintPool, error) {
	cfg, ok := db.MaintConfigFrom(d)
	if !ok {
		return nil, nil
	}
	return db.OpenMaint(ctx, cfg)
}
