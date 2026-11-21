package cli

import "embed"

// SchemaFS 内嵌 envelope 与各命令 data 的 JSON Schema（tools-design §6；CF-5 冻结点：退出码表 + 这些 schema）。
// 命名：envelope.schema.json、data.<命令>.schema.json（命令中的空格以 - 代替）。
// CI 以 schema_test.go 校验真实命令输出与示例、并断言「每个已交付命令都有 data schema」。
//
//go:embed schema/*.json
var SchemaFS embed.FS
