// Package ports 定义 Server 内跨模块协作的端口：仅含接口与 DTO，不含任何实现（ADR-001 §5，IMP-15）。
//
// 规则：
//   - 调用方只依赖 platform/ports；实现方在 cmd/lingedge-server 装配层注入，因此编译期依赖图只增加
//     「所有模块 → platform/ports」这一条允许的边，不存在临时豁免；
//   - 本包不得出现函数/方法实现、包级变量，也不得新增清单之外的接口（`make arch-check` 强制）；
//   - 接口清单（变更须评审并同步 ADR-001 §5 与 scripts/gates/spec.go）：CertificateProvider、SiteDirectory、
//     NodeDirectory、NodeTaskDispatcher、OriginHealthSink、RollupSink、LogSink、LogQuery、OriginGroupLookup、
//     CachePolicyLookup、PlanCatalog。各接口的方法集由其所属 Story 随实现方一并定义。
package ports
