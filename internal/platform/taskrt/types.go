package taskrt

import (
	"regexp"
	"sort"
	"strings"
)

// Type 是任务类型（白名单枚举）。
type Type string

// 任务类型常量：OpenAPI components.schemas.TaskType 的 25 项（唯一事实源：docs/api/openapi.yaml）。
// 新增类型必须先改 OpenAPI 与迁移 CHECK（expand），再追加到这里；registry_test.go 与门禁 tasktype 机器核对三者一致。
const (
	TypeDeploymentPublish   Type = "deployment.publish"
	TypeDeploymentRetry     Type = "deployment.retry"
	TypePublishDispatch     Type = "publish.dispatch"
	TypeCachePurgeURL       Type = "cache.purge_url"
	TypeCachePurgeSite      Type = "cache.purge_site"
	TypeCachePrefetch       Type = "cache.prefetch"
	TypeCachePurgePrefix    Type = "cache.purge_prefix"
	TypeCertIssue           Type = "cert.issue"
	TypeCertRenew           Type = "cert.renew"
	TypeCertDistribute      Type = "cert.distribute"
	TypeNodeUpgrade         Type = "node.upgrade"
	TypeNodeRollback        Type = "node.rollback"
	TypeRealipDebug         Type = "realip.debug"
	TypeSystemBackup        Type = "system.backup"
	TypeDNSSync             Type = "dns.sync"
	TypeRetentionCleanup    Type = "retention.cleanup"
	TypeRollupAggregate     Type = "rollup.aggregate"
	TypeUsageDerive         Type = "usage.derive"
	TypeHeartbeatSweep      Type = "heartbeat.sweep"
	TypeSecretsRewrap       Type = "secrets.rewrap"
	TypeServerCertRotate    Type = "server.cert_rotate"
	TypeMediaCleanupOrphans Type = "media.cleanup_orphans"
	TypeCMSRevalidate       Type = "cms.revalidate"
	TypeCMSPublishDue       Type = "cms.publish_due"
	TypeCMSPrerender        Type = "cms.prerender"
)

// KnownTypes 返回全部已知（OpenAPI 允许）的任务类型，升序。已知 ≠ 已登记：只有经 Registry.Register 的类型才能入队/被执行。
func KnownTypes() []Type {
	out := make([]Type, 0, len(known))
	for t := range known {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// IsKnown 报告 t 是否在 OpenAPI TaskType 白名单内。
func IsKnown(t Type) bool { _, ok := known[t]; return ok }

var known = func() map[Type]struct{} {
	m := map[Type]struct{}{}
	for _, t := range []Type{
		TypeDeploymentPublish, TypeDeploymentRetry, TypePublishDispatch,
		TypeCachePurgeURL, TypeCachePurgeSite, TypeCachePrefetch, TypeCachePurgePrefix,
		TypeCertIssue, TypeCertRenew, TypeCertDistribute,
		TypeNodeUpgrade, TypeNodeRollback, TypeRealipDebug,
		TypeSystemBackup, TypeDNSSync, TypeRetentionCleanup, TypeRollupAggregate, TypeUsageDerive, TypeHeartbeatSweep,
		TypeSecretsRewrap, TypeServerCertRotate, TypeMediaCleanupOrphans,
		TypeCMSRevalidate, TypeCMSPublishDue, TypeCMSPrerender,
	} {
		m[t] = struct{}{}
	}
	return m
}()

// forbiddenSegments 是任务类型名中不得出现的「执行任意命令」语义片段（按 . 与 _ 切分后整段相等）。
// 这是纵深防御：类型本身必须先在 KnownTypes 白名单内，此检查保证即使有人误把命令类类型写进白名单，登记也会失败。
var forbiddenSegments = map[string]bool{
	"exec": true, "execute": true, "shell": true, "sh": true, "bash": true, "cmd": true, "command": true,
	"script": true, "eval": true, "spawn": true, "popen": true, "run": true,
}

var reTypeName = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9_]*)+$`)

// ForbiddenTypeReason 若类型名带有「执行命令」语义则返回原因，否则返回空串。
func ForbiddenTypeReason(t Type) string {
	if !reTypeName.MatchString(string(t)) {
		return "类型名格式非法（须为 小写域.小写动作，如 cache.purge_url）"
	}
	for _, seg := range strings.FieldsFunc(string(t), func(r rune) bool { return r == '.' || r == '_' }) {
		if forbiddenSegments[seg] {
			return "类型名含执行命令语义片段 " + seg + "（FR-35：不存在执行任意命令的任务类型）"
		}
	}
	return ""
}

// Status 是任务状态（与 OpenAPI TaskStatus、tasks.status CHECK 一致）。
type Status string

// 任务状态。
const (
	StatusPending  Status = "pending"
	StatusRunning  Status = "running"
	StatusSuccess  Status = "success"
	StatusPartial  Status = "partial"
	StatusFailed   Status = "failed"
	StatusCanceled Status = "canceled"
)

// Terminal 报告是否终态。
func (s Status) Terminal() bool {
	return s == StatusSuccess || s == StatusPartial || s == StatusFailed || s == StatusCanceled
}

// Valid 报告是否合法状态。
func (s Status) Valid() bool { return s == StatusPending || s == StatusRunning || s.Terminal() }
