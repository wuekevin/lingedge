# CF-6 发行清单/包格式冻结评审记录（`internal/tools/manifest`）

- **日期**：2026-11-21
- **对应 Story**：1.27「manifest 与发行信任域①（验签 / sequence / 吊销 / 密钥轮换）」（`docs/bmad/stories/1-27-manifest-与发行信任域①-验签-sequence-吊销-密钥轮换.md`）
- **签收方式**：同 CF-1/CF-2（UD-25）——机器复核全绿 + 开发代理出具评审记录，**未经人工签收**；用户可事后推翻（推翻则 CF-6 作废，以新的契约变更说明修正，并通知 1.28/1.30/1.34 三个并行泳道）。
- **冻结机制**：与 CF-5 同类（无 git 标签）：冻结对象是代码内嵌的 JSON Schema 文件 + 签名域前缀常量，由单测以 SHA-256 钉住（`internal/tools/manifest/schema_files_test.go` 的 `TestSchemaFilesFrozen`、`isolation_test.go` 的域前缀断言）。此后任何改动都必须同时：① 在 `docs/bmad/contract-changes/` 新增变更说明（模板 `_template.md`；破坏性变更须关联 ADR，ADR-017 §10）；② 更新测试里的冻结哈希；③ 通知 downloader（1.28）、resolver（1.30）、发行流水线（1.34）。
- **结论**：**通过**（仅限本机可验证部分；见 §4）。1.28、1.30、1.34 可据此并行。

## 1. 冻结对象

| 对象 | 位置 | 内容 |
|---|---|---|
| 清单 schema（`schema_version=1`） | `internal/tools/manifest/schema/manifest.schema.json`；Go 实现 `manifest.go`（权威） | 顶层/component/version/variant/artifact 字段、必需性、枚举、`critical` 规则、`key_rotation` 段 |
| 公钥集 schema | `schema/keys.schema.json`；`keys.go` | `keys.json` 与 `trust/keys.d/*.json` 共用格式；严格（不允许未知字段） |
| 签名文件 schema | `schema/sig.schema.json`；`verify.go` `ParseSig` | `{"key_id","alg":"ed25519","sig":"<base64 64B>"}` |
| 包 META schema | `schema/package-meta.schema.json`；`pkg.go` | `META/package.json` 字段 |
| 签名对象 | `verify.go`/`pkg.go` 常量 | 清单：`"LINGEDGE-MANIFEST-V1\n"` ‖ `manifest.json` **原始文件字节**；包：`"LINGEDGE-PKG-V1\n<component>\n<version>\n<platform>\n"` ‖ `META/SHA256SUMS`；轮换背书：`"LINGEDGE-KEYROT-V1\n"` ‖ `key_id\nalg\npublic_key\nnot_before\nnot_after\nstatus\n` |
| 吊销拆分 | tools-design §9.5 | `keys.json.revoked_key_ids`（及 `keys.d` 内同名字段）∪ 清单 `revoked_versions`；清单内**不含** `revoked_key_ids` |
| 包格式 | tools-design §9.3 | `.tar.gz`：`META/package.json`、`META/SHA256SUMS`、`META/SHA256SUMS.sig`、`payload/…` |
| golden 向量 | `internal/tools/manifest/testdata/vectors.json` | tools-design §9.6 的真实计算示例（仅测试，非发行密钥），供 1.34 交叉验证 |

## 2. 冻结前决策（对 tools-design §9 原文的补全与取舍，已回写 §9）

1. **「未知必需字段」的可判定定义**：原文只写「未知必需字段拒绝，未知可选字段忽略」，没有说明解析方如何知道某个未知字段是「必需」的。采用 X.509 critical 扩展的做法：对象内 `critical: [字段名…]`，解析方不认识其中任一名字即拒绝；未列入的未知字段忽略。可出现在五个层级。
2. **`key_rotation` 的具体形态**：原文只写「新公钥 + 由旧 active key 签名的背书」。定为 `{keys[], endorsements[{key_id, endorsed_by, sig}]}`，背书签名输入含新增的第三个域前缀 `LINGEDGE-KEYROT-V1\n`（域分隔，与清单/包/Server 的 REVISION/KEYSET 互不相同）。
3. **sequence 按通道记录**：原文是单个「已见最大 sequence」。stable 与 beta 若各自独立编号，单值会让 beta 阻断 stable；按通道记录在两种发行编号策略下都安全（单调编号时更严格的是通道内比较，不会放行回滚）。
4. **公钥有效期窗口以清单 `generated_at` 判定**（而非当前时间）：离线 bundle/旧清单不因公钥后来到期而无法验证；泄露靠吊销而非时钟。
5. **schema 收紧点**：`revoked_versions` 必填（缺失即拒绝，防止静默无吊销）；每个版本必须有 `variants`/`artifacts`；`size ∈ [1,8GiB]`；`url` 仅不含凭据的 https；重复 JSON 键/超深嵌套/超 8MiB 拒绝。§9.2 示例里的 `0`/`<hex>`/`…` 是格式示意，真实清单不允许。
6. **semver 约束库与语法**：`github.com/Masterminds/semver/v3` 固定 v3.5.0；版本用 `StrictNewVersion`；约束语法限定为 tools-design §10 的子集（逐 token 预校验，库额外支持的通配/`!=`/连字符范围/逗号一律拒绝）；预发布语义固定为库行为并有边界测试。

## 3. 复核项与实测结果

| 项 | 结果 | 证据 |
|---|---|---|
| §9.6 golden 向量：种子→公钥→签名确定性复算与文档一致；生产验签路径通过；任一字节翻转（清单/签名/SHA256SUMS）失败 | 通过（Go `crypto/ed25519`） | `golden_test.go` |
| 篡改清单（逐字节翻转）/篡改签名/未知 `key_id`/被吊销 key/回滚/过期/`--allow-expired-manifest`/`revoked_versions`/`key_rotation`（旧 active 背书） | 通过 | `verify_test.go`、`rotation_test.go`、`trust_test.go` |
| semver 边界（严格解析、优先级链、约束算符边界、`^`/`~`/`||`/预发布、语法子集拒绝）+ fuzz | 通过 | `version_test.go` |
| 退出码 5 真实生产者（真实 Verifier 错误 → `Run.finish` → envelope `exit_code=5`） | 通过 | `internal/tools/cli/verifyerr_test.go` |
| 构建守卫（缺失 → 发行构建失败；含测试公钥 → 任何构建失败；真实 `go build -overlay` 二进制字节扫描 + 负向样本） | 通过 | `scripts/gates/releasekeys_test.go`、`tests/gates`、`tests/security/releasekeys` |
| 信任域隔离（不 import `pkg/signing`、域前缀互异、包内无任何签名/私钥代码；`gates trustdomain` 与二进制符号层检查仍通过） | 通过 | `isolation_test.go`、`make security` |
| schema 文件被钉住且与真实向量/Go 实现一致 | 通过 | `schema_files_test.go` |
| 变异验证（临时去掉每一道关键检查，测试须失败，再还原） | 见 Story 文档「变异验证」 | — |

## 4. 未覆盖 / 仍开放

- **真实发行公钥**（UD-3，发行签名人/硬件密钥）：仓库内没有任何发行公钥；生产构建必须由流水线注入 `builtin/keys.json`。真实发行与真实安装验收仍被 UD-3 阻塞（不阻塞开发）。
- **第二实现交叉验证**：§9.6 声称的 Ruby OpenSSL 验签结论沿用文档；本机 Ruby 2.6/LibreSSL 不支持 Ed25519，未能在本机复验。Go 侧已独立复算并验签；Ruby 侧交叉验证随 1.34 流水线（其流水线自身的签名器会产出同口径向量）进行。
- **下游尚未消费**：downloader（1.28）、resolver（1.30）、registry（1.29，存 sequence 的事务边界）、流水线签名器（1.34）按本冻结并行开发；本记录不替它们验收。
- 缺少独立人工评审（UD-25 默认接受）。
