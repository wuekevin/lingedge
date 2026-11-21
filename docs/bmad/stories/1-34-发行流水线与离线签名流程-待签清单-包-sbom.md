---
title: 'Story 1.34：发行流水线与离线签名流程（待签清单 / 包 / SBOM）'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: 'f8ff810'
waiting_env_verification: true
context:
  - docs/bmad/epics.md Story 1.34（AC1–AC6）
  - docs/prd/LINGEDGE_V2_REQUIREMENTS.md（NFR-15、FR-9、FR-26、NFR-12）
  - docs/architecture/adr/ADR-012-release-signing.md §1、§10；docs/architecture/tools-design.md §9（含 §9.6 向量）；docs/architecture/dev-environment.md §3/§4
  - internal/tools/manifest（Story 1.27、CF-6）、internal/tools/downloader（Story 1.28）、scripts/gates/releasekeys.go
---

## Intent

**Problem:** 1.27/1.28 做好了 Tools 侧的验签与取包链，但「被验证的东西」从哪来没人负责：`make release` 仍是 `NOT_IMPLEMENTED(story 1.34)`，没有产出清单与包的流水线，没有离线签名工具，「仓库与 CI 中不存在发行私钥」只是文字约束，SBOM 与可复现性没有记录，§9.6 向量只在 Go 一侧被验证过（Ruby 侧沿用文档里的结论），OpenResty 目录里的符号链接与「SafeExtract 拒绝链接」之间没人做构建期消除。

**Approach:** 两个独立程序 + 一个门禁 + 一套端到端测试。**`scripts/release-sign`**（独立 Go module，仅标准库，离线机运行）负责 keygen/sign/verify/endorse，签名输入是对 tools-design §9 的**独立实现**；**`scripts/release`**（在线流水线，无任何签名代码）分 `build` → `finalize` → `verify` 三段，产出待签清单/包/SBOM/可复现性记录，并在 `verify` 里用 **Tools 同一条校验链**（`manifest.Verifier` + `downloader.Acquire` 的离线 bundle 路径）验证将要发布的字节；**`gates secrets`** 把「仓库与 CI 无私钥」变成 `make security` 的门禁（无豁免）。两轮离线签名（先包内 `SHA256SUMS`，再清单）是被「包内签名在包里面、包的外层 sha256 在清单里」这一循环依赖逼出来的，见「设计偏差」。

## 逐条验收

- **AC1 `make release` 产出包与待签清单**：`make release`（先过 `release-keys-check RELEASE=1`，缺失/含测试公钥即失败）→ `scripts/release build`：构建 server/tools × linux-amd64/arm64（`-trimpath -buildvcs=false CGO_ENABLED=0`，无 `lingedge_testdouble`，版本与提交经 ldflags 注入）→ 取 `lingedge-server migrate plan --offline` 的迁移预告（`schema_from/schema_to/classes/irreversible_count/backup_required`，`TestParseMigratePlan`、`TestPublishLayoutAndManifestContent` 与独立执行的 migrate plan 对账）→ 组装 stage（`META/package.json`、`META/SHA256SUMS`、`payload/…`，每包带 `payload/share/lingedge-sbom.cdx.json`）→ `to-sign-1.json`。`make release-finalize` 之后得到最终 `.tar.gz`（确定性、META 在前，`TestWritePackageDeterministicAndOrdered`）与待签 `manifest.json`（`sequence/valid_until/ports/requires/migrations 预告`，生成后立即用 `manifest.ParseManifest` 自检）。**生产构建不带 `lingedge_testdouble`**：`TestProductionBuildFlags` 对产物做 `go version -m`（无 `-tags`、`-trimpath=true`、无 vcs 戳）、字节级检索无测试替身包与测试公钥。
- **AC2 离线签名工具与「仓库/CI 无私钥」**：`scripts/release-sign`（独立 module、仅标准库、不链接 net/http：`TestReleaseSignIsStandalone`）。`sign` 对待签清单逐项核对（文件 SHA-256 与清单一致、路径不逃逸/不含符号链接、`SHA256SUMS` 行格式与 `files_count`、清单 schema_version/通道/sequence/有效期/`revoked_versions` 必填/URL 必须是不含凭据的 https/拒绝空清单），显示人读摘要，人工键入 `sign` 后才产出 `.sig`；私钥文件 0600/`O_EXCL`，加载时拒绝符号链接与 group/other 可访问；任一核对失败不产出任何 `.sig`。**密钥扫描 G-7**：`gates secrets`（`make security`，无豁免）——PEM 私钥块（且正文是真实密钥结构：DER 外层长度自洽/OpenSSH 魔数/PGP 足够长，占位正文如 `MIIB`/`AAAA` 不误报）、离线签名工具私钥文件、PKCS#8 Ed25519 DER、private_key 类键的种子长度高熵值、私钥类文件名、CI 配置引用发行私钥；负向样本在运行时生成真实密钥（`scripts/gates/secrets_test.go`）。在线流水线没有任何签名代码：`TestOnlinePipelineHasNoSigningCode`。
- **AC3 SBOM 与可复现性**：CycloneDX 1.5，Go 用 `go version -m`（每个实际发行二进制的 toolchain/构建设置/全部依赖模块与校验和），npm 用 `pnpm-lock.yaml`（当前仓库无前端工程，构建记录如实写「无 lockfile」，`TestNPMLockSBOM` 用真实 lockfile 夹具覆盖解析）；每包 SBOM 在 payload 内（受包内签名与清单 sha256 保护）并有 `publish/SBOM.cdx.json` 汇总；SBOM 无非确定内容（同一二进制两次生成逐字节相同）。**可复现性**：固定 toolchain（`go.mod` 的 `toolchain` 行）、`-trimpath`、`-buildvcs=false`；`make release` 总会把源树复制到另一目录并用**干净 GOCACHE** 再构建一遍，逐文件比对 SHA-256 写入 `repro.json`，不一致即失败（`compareBuilds` 的负向用例、`TestReproducibilityRecord`）。验证记录见下节。
- **AC4 发行源可配置与运维文档**：`RELEASE_BASE_URL`（`finalize -base-url`，必须是不含凭据/查询/片段的 https，`TestCheckBaseURLAndRevoke`、`TestFinalizeRejectsBadFirstRoundSignatures`）决定清单内所有包 URL；发行目录布局 `<通道>/manifest.json(.sig)`、`<组件>/<版本>/<文件>` 与 Tools 侧 `release.base_url` 的 `ManifestURL()` 约定对账（`TestPublishLayoutAndManifestContent`）。`docs/operations/release.md` 记录签名人/保管/吊销/轮换/泄露处置/两轮流程（UD-3 项标「待确认」）。
- **AC5 含测试公钥的 keys.json 生产构建失败**：`make release` 先过 `release-keys-check RELEASE=1`；流水线 `build` 内部再调用 `gates release-keys -require` 并扫描产物字节里的测试公钥（原始/base64/hex，`TestScanBinaryForTestKeys`）。`TestReleaseBuildRejectsBadBuiltinKeys`：golden 测试公钥、test 字样 key_id、含 seed 字段、缺失 keys.json、脏工作树各一例，均在产出任何 stage 之前失败。
- **AC6 §9.6 向量交叉验证与符号链接消除**：`scripts/release-sign` 的独立实现复算出 §9.6 的清单签名与包签名**与文档逐字节相同**（`TestGolden96ReproducedBySigner`，读取 1.27 落盘的 `vectors.json`）；**本机真实运行 Ruby OpenSSL**（Homebrew Ruby 4.0.7 + OpenSSL 3.6.4）：Ruby 与 Go 对 §9.6 清单/包签名均验签通过，篡改/域混淆/去域前缀均失败且两实现判定逐项一致，Ruby 对同一种子的确定性签名与文档逐字节相同，运行时随机密钥 Go 签 ↔ Ruby 验、Ruby 签 ↔ Go 验双向通过（`TestGolden96VerifiedByRubyOpenSSL`、`TestRubyOpenSSLSignsSameAsGo`）；流水线真实产出的签名（清单 + 各包 SHA256SUMS）同样由 Ruby 验证（`TestRubyOpenSSLVerifiesPipelineSignatures`）。符号链接：`treeCopier` 在构建期把链接物化为普通文件/子树，拒绝逃逸/悬空/循环/特殊文件/不合 SafeExtract 路径规则/仅大小写不同重名；最终 payload 残留任何链接即失败；端到端 `TestOpenRestyTreeSymlinksEliminated`（真实 OpenResty 风格目录树 → 包内只有普通文件 → 经 Tools 校验链取出无链接；逃逸/悬空/目录环使构建失败）；`TestSafeExtractStillRejectsLinks` 证明 SafeExtract 未放宽。

## 验证记录

**可复现性（真实 `release build`，演练）**：干净提交、`-version 0.1.0`、4 个二进制（lingedge-server、lingedge × linux-amd64/arm64），第二次构建在另一源目录 + 干净 GOCACHE，toolchain go1.27.4。`repro.json`：

| 二进制 | 平台 | sha256（两次构建相同） |
|---|---|---|
| lingedge-server | linux-amd64 | `a14704600201e2ee67b97f08f7c6dca83be2e3f22d73baca7c38092c841d9f07` |
| lingedge-server | linux-arm64 | `df4c46788008336d3bbdd15cff9031e93e97bcde74b59a1d8bfbc59b603d112a` |
| lingedge | linux-amd64 | `52aa3f716411432b46d0c9f1fadb399ac6543e93dfc8e9ec279f93422bb86d4f` |
| lingedge | linux-arm64 | `617aba204f39aa18ed78d6458f88d6da90e2fe842ee0b6a99aaf89f1462300da` |

`all_equal: true`。注：哈希含提交哈希（ldflags 注入），演练使用的是工作树快照提交 `d05fb45…`；复现性结论针对「同一提交、同一 toolchain、不同目录与缓存」。每次真实发行的 `repro.json` 随 `publish/` 归档。

**端到端（`tests/release`，真实 go build + 真实离线签名工具子进程 + 真实 Tools 校验链；测试密钥运行时随机生成）**：完整流程 build → 第 1 轮签名 → finalize → 第 2 轮签名 → verify 通过；Tools 验签通过（`manifest.Verifier` + `downloader.Acquire` 对 server/tools × 2 平台逐个取包并解压，二进制 0755、版本/提交已注入、SBOM 合法）；篡改被拒（清单字节、签名字节、非公钥集内的签名者 → `unknown_key_id`、伪造 key_id → `signature_invalid`、sequence 回滚、过期、通道不符、密钥吊销、包体字节翻转 → sha256 不符、包错位）；流水线 `finalize`/`verify` 同样拒绝（缺签名、非签名者、签名后 stage 被调包、sequence 不严格递增、非 https/带凭据发行源、包体/清单/签名被改、包缺失），且失败时不产出 `publish/`；公钥轮换端到端（旧 key 背书 → Tools 验证并落盘 keys.d → 新 key 签名的清单通过；无 keys.d 时 `unknown_key_id`；未知背书者 → `rotation_invalid`）。

## 测试

- `scripts/release-sign`（独立 module，`make release-sign-test`）：keygen/sign/verify/endorse 往返、拒绝非法输入表（http/带凭据 URL、缺 revoked_versions、文件被改、符号链接、空清单、非 JSON、valid_until 不晚于 generated_at、路径逃逸、SHA256SUMS 格式）、私钥文件防护（权限/符号链接/格式）、交互确认与 `-expect-digest`、§9.6 复算、Ruby 交叉验证（Ruby 缺失时明确跳过并标「等待环境验证」，`LINGEDGE_REQUIRE_RUBY=1` 失败；`make` 缺省为 1）。
- `scripts/release`：符号链接消除（相对/绝对/链中链/目录链接/硬链接/兄弟目录链接合法；逃逸/悬空/环/FIFO/大小写重名/百分号编码名/过深必须拒绝）、`parseMigratePlan`（含矛盾输入）、`compareBuilds`、`go version -m` 解析与 SBOM 确定性、pnpm lockfile、`checkBaseURL`/`parseRevoke`、确定性打包与 stage 调包检测、产物测试公钥扫描、在线流水线无签名代码且依赖闭包不含 Server 业务模块。
- `scripts/gates/secrets_test.go`：真实随机生成的 PEM（PKCS#8/EC/加密）与 OpenSSH 形态、转义换行形态、占位正文不误报、签名工具私钥文件、PKCS#8 base64、private_key 字段（含路径/短值/公钥/DER 固定头不误报）、文件名与 CI 规则、整树运行。
- `tests/release`：见上；共 14 个测试函数（含 `TestRepoSecretScanPasses`、`TestSafeExtractStillRejectsLinks`）。

## 变异验证

对关键防线做临时改动 → 测试必须失败 → 还原并核对无残留：finalize 的包内签名验证、finalize 的「stage 与已签名 SHA256SUMS 一致」、符号链接逃逸检查、目录环检查、`compareBuilds` 的空哈希、`release-sign` 的文件 SHA-256 核对、URL 必须 https、私钥权限检查、包签名输入缺 platform、清单域前缀改版本、`gates secrets` 的 PEM 规则与 DER 自洽检查、`-trimpath` 被去掉、产物测试公钥扫描（base64/hex）、`listPayload` 的残留链接检查，共 16 条**全部被检出**。另有 2 条等价变体不算：悬空链接的专门报错被去掉后仍被下一道「目标在源树之外」拦截；`verify` 里吞掉 Acquire 错误会因 nil 解引用崩溃使流水线仍失败（且另有 artifact 计数校验）。过程中发现「产物测试公钥扫描」初始没有直接测试，已补 `TestScanBinaryForTestKeys`。

## 对既有测试/防护的改动（逐条，均为加强，无放宽）

| 文件 | 改动 |
|---|---|
| `Makefile` | `release` 由 `NOT_IMPLEMENTED(story 1.34)` 占位改为真实目标，新增 `release-finalize`、`release-verify`、`release-sign`、`release-sign-test`、`release-test`、`secrets-check`；`security` 增 `$(GATES) secrets`；`gate-selftest` 增 `release-sign-test`；`security` 的说明文字与 release-keys 注释更新。 |
| `scripts/gates/main.go` | 注册 `secrets` 门禁（`all` 与帮助文本）。 |
| `tests/scaffold/scaffold_test.go` | `release` 从「必须是 NOT_IMPLEMENTED」清单移到「必须存在且可解析」清单，并加入新目标；`security` 必须包含 `scripts/gates secrets`；`gate-selftest` 必须包含 `release-sign-test`；新增 `release` 配方必须先经 `release-keys-check RELEASE=1` 再进入流水线（防止被绕过）。 |
| 文档 | tools-design §9.6 的 Ruby 结论由「沿用文档」更新为本机真实复验；tools-design/ADR-012 追加 2026-11-21（Story 1.34）修订记录；dev-environment §3 的 `make release` 行；threat-model G-7 行；新增 `docs/operations/release.md`。 |

**未改动**：`SafeExtract`（及其任何限额/类型白名单）、既有门禁规则与词表（`release-keys`、`testdouble`、`passwords`、`trustdomain` 等）、`.golangci.yml`、`internal/tools/**`（manifest/downloader/security 等一行未改）、CF-6 schema 与 `contract-v1`、迁移（`migrations-m1`）、OpenAPI/proto。

## 与任务/通常做法不同的设计决定（须披露）

1. **两轮签名，AC1 的「待签 manifest.json」在 `finalize` 而非 `make release` 产出**：包内 `META/SHA256SUMS.sig` 在 `.tar.gz` 内部，包的外层 sha256 写进清单，清单再被签名——最终清单必然晚于包内签名。所以 `make release` 产出 stage 树（含 `META/package.json`、`META/SHA256SUMS`、`payload/`）与第 1 轮待签清单；`make release-finalize` 在验证第 1 轮签名后写出最终 `.tar.gz` 与待签 `manifest.json`；签名人需要在离线机做两次签名。未选择「离线签名工具也负责组装」，那会让离线机承担 tar/清单生成逻辑，扩大其需要被信任的代码面。
2. **迁移预告用 `migrate plan --offline`，不用 `--json`**：该子命令不接受 `--json`（输出默认即 JSON）；不改 server 代码，取离线模式（列出全部内嵌迁移）。
3. **SBOM 不用外部工具**：`go version -m` + lockfile 解析自己生成 CycloneDX，避免引入 syft/cyclonedx-gomod 等需要预装/联网的依赖并保证可复现。
4. **`make release` 不调用 `make build RELEASE=1`**：两者共用同一守卫 `release-keys-check RELEASE=1`；但流水线用发行版本（而非 `git describe`）注入 ldflags 且按组件目录构建，复用 `dist/` 会导致二进制与发行版本不一致。
5. **组件目录表如实标状态**：只有 server、tools 为 `available`；node/dns/reporter/openresty/前端为 `unreleased`（对应交付物尚不存在），scheduler/worker 为 `builtin`。不发布空壳包。
6. **输出目录缺省在 `bin/release/<版本>/`**（被各门禁跳过、不入库）；`make clean` 会删除它，发行物发布后应归档。
7. **端到端测试的第二次构建不用干净 GOCACHE**（`-repro-fresh-cache=false`，只换源目录）以控制耗时；干净 GOCACHE 的完整验证见「验证记录」，且 `make release` 缺省开启。

## [ASSUMPTION]

- server 的 `requires`（最低内存 1024MB/磁盘 2048MB、MySQL `>=8.0.16 <9`、Redis `>=8`）与端口表取自 tools-design §9.2 示例与 `configs/server.example.yaml`，发行负责人正式发行前复核；`config_schema_version=1`。
- `backup_required`：含 reversible/irreversible 迁移即 true，纯 expand 为 false（以 `migrate plan` 实际结果为准、取更严格者，§17.2）；`min_upgrade_from` 缺省等于本版本（`-min-upgrade-from` 可改）；`valid_until` 缺省 generated_at+90 天；`sequence` 由发行负责人显式指定。
- 发行源布局：清单 `<base_url>/<通道>/manifest.json(.sig)`（与 1.33 一致的 [ASSUMPTION]）；包 `<base_url>/<组件>/<版本>/<文件名>`；包文件名 `lingedge-<组件>-<版本>-<os>-<arch>.tar.gz`。
- 包内权限位：`payload/bin/*` 为 0755，其余 0644，仅作安装器约定的来源（META 无「可执行」字段，安装器按 `ComponentSpec.Executables` 最终决定）；每包 SBOM 位置 `payload/share/lingedge-sbom.cdx.json`；server 包带 `share/config/server.example.yaml`。
- 密钥命名 `rel-<年>-<序>`、有效期窗口 2 年、双人复核、泄露处置步骤均为 `docs/operations/release.md` 的建议默认值，**UD-3 待用户确认**。
- `gates secrets` 识别「真实私钥形态」，不把任意 32 字节十六进制当私钥：§9.6 的**公开**测试种子（`vectors.json`、tools-design §9.6）是文档化的公开向量，其公钥登记在 `test-release-pubkeys.txt`，由 G-11 与 release-keys 保证不进生产——这是门禁的识别边界，不是豁免（未给任何文件/路径开例外）。

## 等待环境验证（代码已完整，待环境）

| 项 | 说明 |
|---|---|
| 真实发行密钥签名、签名人/保管介质/应急渠道、公开发行源（**UD-3**） | 流水线、离线签名工具与测试密钥闭环本机完成并经测试；真实密钥与公开源待用户确认后按 `docs/operations/release.md` 执行。确认前不得对外发行。 |
| 真实 OpenResty 目录树的打包（来源方案 **UD-4**） | 构建期链接消除、`-openresty-tree` 接线与端到端（真实文件系统上的 OpenResty 风格目录树 → Tools 校验链）已完成；真实 OpenResty 二进制与来源/SHA256 登记属 Story 3.22 的交付物，到位后只需 `make release RELEASE_FLAGS='-openresty-version … -openresty-tree …'`。 |
| 远端 CI 上的 `make gate-selftest`（含 Ruby 交叉验证） | 本机已真实运行；`ci.yml` 仍未在远端实际运行过（仓库无远端），CI 镜像需带支持 Ed25519 的 Ruby（OpenSSL ≥1.1.1），否则该步失败（`RELEASE_REQUIRE_RUBY=0` 可降级为跳过并标等待环境验证）。 |

## 属后续 Story（依据）

| 项 | 归属 | 依据 |
|---|---|---|
| `cmd/lingedge` 链接 `internal/tools/manifest` 后，发行二进制才真正 `go:embed` 内置 `keys.json`；届时守卫对真实 `lingedge` 产物生效（流水线的产物扫描与 `tests/security/releasekeys` 已就绪） | **Story 1.35**（Tools 命令接线、`install server`） | 当前 `cmd/lingedge` 仅 `--version` 脚手架 |
| `lingedge install server` 消费本流水线的 server 包（`payload/bin/lingedge-server`、`share/config/…`、`Executables`、`ComponentSpec`），以及从发布目录/bundle 真实安装 | **Story 1.35**、1.45（ENV-LNX 验收） | 1.33 把 `ComponentSpec` 实现留给 1.35 |
| 升级路径消费同一套清单/包、Tools 自升级（`tools` 包的原子替换） | **Story 1.43** | epics 1.43 |
| 前端组件包（`admin-web/console/website/docs/status`）与 npm SBOM/lockfile | **Story 1.36** 及后续前端 Story | 前端 workspace 属 1.36 |
| node/dns/reporter/openresty 的真实包与「unreleased → available」 | Epic 3（Story 3.22 起）等对应 Story | 对应交付物尚不存在 |
| 许可证扫描（G-7 子项）、发行私钥泄露响应与密钥轮换的**演练记录** | **Story 6.19**（安全审计） | epics 6.19 AC2 |
