// Package migrations 以 embed.FS 嵌入全部迁移文件（ADR-016 §1、Story 1.6）：迁移随 lingedge-server 二进制发行，
// Tools 不单独携带 SQL。本包只导出文件系统；解析、lint 与执行在 internal/platform/migrate。
//
// 目录内允许的非迁移文件：CHECKSUMS（已发布迁移的哈希清单，Story 1.3）与本文件；迁移文件命名 NNNNNN_<name>.sql。
// 已发布（CHECKSUMS 已登记）的迁移只追加、不得修改（make migrations-check / contract-check）。
package migrations

import "embed"

// FS 是嵌入的迁移文件（根目录平铺）。模式用 `*` 而非 `*.sql`：迁移尚未落库的阶段 `*.sql` 无匹配会使编译失败。
//
//go:embed *
var FS embed.FS
