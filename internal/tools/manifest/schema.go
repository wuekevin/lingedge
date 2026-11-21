package manifest

import "embed"

// SchemaFS 内嵌 CF-6 冻结的 JSON Schema：manifest、keys、sig、package-meta（机器可读镜像；Go 实现为权威，schema_test.go 用它校验真实向量并断言两者一致）。
// 任何 schema 变更都属 CF-6 契约变更，须新增 docs/bmad/contract-changes/ 变更说明（schema_freeze_test.go 以哈希钉住）。
//
//go:embed schema/*.json
var SchemaFS embed.FS
