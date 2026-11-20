// Package leakguard 是规则 R-T1（ADR-009 §5、BLK-02）的响应字段断言器：租户可见（x-tenant-scope 为 tenant|either）的真实响应
// 不得含节点/集群的内部标识。
//
// 与 Story 1.3 的 OpenAPI 静态扫描（scripts/contract 的 tenant-leak）互补：静态扫描检查契约里声明的属性名，本包检查**真实响应体**——
// 键名（node_*、cluster_*、egress_node*、targets、nodes/clusters 列表…，与静态扫描同一正则并更严）、调用方登记的内部取值
// （节点主机名/IP/uid、集群名称等的子串，大小写不敏感）、内部错误全文特征（SQL/驱动错误、goroutine 栈、源码路径…）。
// 聚合计数（如 summary.healthy/total/offline）允许：它们的键名不命中规则。
//
// 它是通用助手：隔离矩阵（tests/security/tenant_isolation）对每个租户可见响应调用 Scan；业务模块在写入租户可见的自由 JSON
// （如 tenants.settings）时用 ForbiddenKey 拒绝带内部标识的键。自测见 leakguard_test.go（正反样本）。
package leakguard
