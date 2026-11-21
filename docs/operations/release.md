# 发行流程与离线签名（Story 1.34）

> 适用：LingEdge 发行负责人与发行签名人。依据：ADR-012 §1（Ed25519 + SHA-256、私钥离线）、tools-design §9（清单/包格式与签名输入）、dev-environment §3/§4、威胁模型 G-7/G-8。
> **状态**：流水线与测试签名闭环在本机完成并经测试；**真实发行密钥、签名人、保管介质与公开发行源待 UD-3**（见 §9）。本文中带 `UD-3 待确认` 的条款是**建议默认值**，用户确认前不得当作既定事实。

## 1. 信任模型（一页纸）

- 发行私钥**只存在于离线机/硬件介质**，不进仓库、不进 CI、不在 Server 上（ADR-012 §1.5，威胁模型 §10）。仓库密钥扫描（`make security` 的 `gates secrets`，G-7）持续证明这一点；测试用的私钥一律在测试运行时随机生成，**不得入库、不设豁免**。
- 在线流水线（`scripts/release`）**没有任何签名代码**（`TestOnlinePipelineHasNoSigningCode` 机器核对），只产出「待签物」并在签名回传后组装、验证。离线签名工具（`scripts/release-sign`）是独立 Go module、只依赖标准库、不联网（`TestReleaseSignIsStandalone`）。
- Tools 内置公钥集 `keys.json`（`go:embed`）是信任根。发行构建**必须**注入真实公钥集；缺失或含测试公钥/测试 key_id/私钥字段，`make release`（以及 `make build RELEASE=1`）直接失败（Story 1.27 AC5，`gates release-keys -require`），流水线 build 内部再检一次并扫描产物字节。

## 2. 角色与保管（UD-3 待确认）

| 项 | 建议默认值（UD-3 待用户确认） |
|---|---|
| 发行负责人 | 触发 `make release*`、核对待签清单摘要、发布到发行源；**不持有私钥** |
| 发行签名人 | 至少 1 人（建议 2 人：1 主 + 1 备份人）；只在离线机运行 `release-sign`，逐项核对后确认 |
| 私钥介质 | 离线机本地加密盘或硬件密钥；私钥文件 0600；离线机不联网、不装其他软件；建议至少 2 份加密备份分处保管 |
| 密钥标识 | `rel-<年>-<序>`，如 `rel-2026-a`；`status` 取 `active`/`retired`；有效期窗口 `not_before/not_after`（建议 2 年） |
| 双人复核 | 建议：签名前由发行负责人与签名人通过带外渠道核对 `to-sign-N.json` 的 SHA-256（`-expect-digest`） |

## 3. 产物与目录布局

`make release` 的输出目录（缺省 `bin/release/<版本>/`，须不存在或为空）：

```
plan.json  build-record.json  repro.json  SBOM.cdx.json   # 构建记录、可复现性记录、汇总 SBOM
stage/<组件>-<版本>-<平台>/{META/package.json, META/SHA256SUMS, payload/…}
to-sign-1.json  signatures/                               # 第 1 轮待签（包内 SHA256SUMS）
--- finalize 之后 ---
packages/<组件>/<版本>/lingedge-<组件>-<版本>-<平台>.tar.gz   # 最终包（META 在前，确定性）
manifest.json  to-sign-2.json                             # 待签清单（第 2 轮）
--- verify 之后 ---
publish/<通道>/manifest.json(.sig)  publish/<组件>/<版本>/*.tar.gz
publish/lingedge-offline-<版本>.tar  publish/SBOM.cdx.json  publish/RELEASE-RECORD.json  (+build-record/repro)
```

- 包格式严格遵循 tools-design §9.3：`META/package.json`、`META/SHA256SUMS`、`META/SHA256SUMS.sig`、`payload/…`，META 三个文件在归档前部；`SHA256SUMS` 只列 `payload/`；`files_count`/`unpacked_size` 与解压限额口径一致；包内只有普通文件与目录，可执行位由 payload 文件模式带出（`bin/*` 为 0755，其余 0644；安装器按组件约定最终决定）。
- 文件名：`lingedge-<组件>-<版本>-<os>-<arch>.tar.gz`（ADR-012 §1.3）；`any` 平台为 `lingedge-<组件>[-<变体>]-<版本>.tar.gz`。
- 清单：`stable` 与 `beta` 是独立通道；清单地址为 `<release.base_url>/<通道>/manifest.json`（`.sig` 在其后），包地址为 `<release.base_url>/<组件>/<版本>/<文件名>`。发布时把 `publish/` 下的内容原样放到发行源即可。
- **哪些组件有包**由 `scripts/release/components.go` 的目录表决定，状态如实：当前 `server`、`tools` 为 `available`；`node/dns/reporter/openresty`（等待 Epic 3 的真实交付物）与前端各组件为 `unreleased`；`scheduler/worker` 为 `builtin`。**不为「看起来完整」发布空壳包。** 某组件交付物落地后，只需改该表。

## 4. 标准流程（两轮离线签名）

为什么两轮：包内 `META/SHA256SUMS.sig` 在 `.tar.gz` 里面，包的外层 sha256 写进清单，清单再被签名——必须先有包内签名，才有最终包与清单。

```
# 在线机 / CI（无私钥）
make release RELEASE_VERSION=0.1.0 RELEASE_KEYS=/path/to/keys.json      # ① 构建 + 可复现性 + stage + to-sign-1.json
#   输出会打印 to-sign-1.json 的 sha256（带外告知签名人）

# 离线机（有私钥；拷入：to-sign-1.json 与其引用的 stage/…/META/SHA256SUMS）
release-sign sign -key rel-2026-a.key.json -to-sign <目录>/to-sign-1.json -expect-digest <sha256>   # ② 第 1 轮
#   核对摘要后键入 sign 确认；产出 signatures/<组件>-<版本>-<平台>.sig；带回在线机

# 在线机
make release-finalize RELEASE_VERSION=0.1.0 RELEASE_SEQUENCE=<N> RELEASE_BASE_URL=https://<发行源> \
     [RELEASE_CHANNEL=stable] [RELEASE_FINALIZE_FLAGS='-valid-days 90 -prev-manifest <上一份> -revoke node@0.0.9=原因']   # ③
#   用 RELEASE_PUBKEYS 验证第 1 轮签名 → 最终包 → manifest.json + to-sign-2.json（打印摘要）

# 离线机
release-sign sign -key rel-2026-a.key.json -to-sign <目录>/to-sign-2.json -expect-digest <sha256>   # ④ 第 2 轮（清单）
#   产出 manifest.json.sig；带回在线机

# 在线机
make release-verify RELEASE_VERSION=0.1.0                                # ⑤ Tools 同一条校验链验证 → publish/
# 通过后把 publish/ 发布到发行源；归档 RELEASE-RECORD.json
```

签名人在 ② ④ 看到的是**人读摘要**（组件/版本/平台、文件数、清单通道/sequence/有效期/每个包的大小与 sha256 前缀）；`release-sign` 在签名前还会：核对每个待签文件的 SHA-256 与待签清单一致、路径不得逃逸/含符号链接、`SHA256SUMS` 行格式、清单的 schema_version/通道/sequence/有效期/`revoked_versions` 必填/URL 必须是不含凭据的 https/不接受空清单；任一项不通过**拒绝签名且不产出任何 `.sig`**。真实签名必须人工键入 `sign`（`-yes` 仅用于自动化演练）。

### 4.1 `sequence` 与有效期

- `sequence` 是每个通道独立单调递增的整数，由发行负责人显式指定（`RELEASE_SEQUENCE`），不得回退——Tools 对回退硬失败且**没有放行开关**（ADR-012 §1.6）。提供 `-prev-manifest` 时流水线强制严格大于上一份。
- `valid_until = generated_at + valid-days`（缺省 90 天，防冻结攻击）。过期清单 Tools 默认拒绝；气隙环境用 `--offline` + `--allow-expired-manifest`（Tools 侧）。
- `generated_at` 默认取 `finalize` 运行时刻；公钥的有效期窗口以它为准（tools-design §9.1）。

## 5. 离线机准备

```
make release-sign                     # 在可联网的开发机构建 bin/release-sign（仅标准库）；拷到离线机
release-sign keygen -key-id rel-2026-a -key-out /secure/rel-2026-a.key.json -pub-out rel-2026-a.keys.json \
     -not-before 2026-11-01T00:00:00Z -not-after 2028-11-01T00:00:00Z
```

- 私钥文件 0600、`O_EXCL` 创建（不覆盖）；加载时拒绝符号链接、group/other 可访问、格式不符的文件。公钥条目（keys.json 格式）带回在线侧，并入 `RELEASE_KEYS` 指向的公钥集，**随新版 Tools 发行**（内置于 `internal/tools/manifest/builtin/keys.json`，该文件在 `.gitignore` 中，仓库不含任何发行公钥）。
- 私钥备份、轮换演练与恢复演练的周期由签名人制定（UD-3 待确认；建议每年一次演练）。

## 6. SBOM（NFR-15、G-7）

- 工具选型（实现时核对）：**不引入外部 SBOM 工具**，流水线用标准库生成 CycloneDX 1.5 JSON——Go：对每个实际发行的二进制执行 `go version -m`（toolchain、构建设置、全部依赖模块的路径/版本/校验和；这是「二进制里真正有什么」的权威来源）；npm：`pnpm-lock.yaml`（精确版本 + integrity）。当前仓库没有前端工程与 lockfile，构建记录如实写「无 lockfile」，不虚构条目。
- **随清单发布且受签名保护**：每个包的 SBOM 在 `payload/share/lingedge-sbom.cdx.json`，被包内 `SHA256SUMS` 签名与清单的外层 sha256 覆盖；`publish/SBOM.cdx.json` 是汇总副本（便于阅读，sha256 记在 `RELEASE-RECORD.json`）。SBOM 不含非确定内容（时间戳取提交时间，serialNumber 由内容摘要派生）。
- 许可证扫描与漏洞扫描：漏洞见 `make vuln`（`govulncheck`，需访问 vuln.go.dev，不在 `make security` 内）；许可证扫描尚未选型接入（G-7 的该子项仍开放）。

## 7. 构建可复现性

- 固定 toolchain（`go.mod` 的 `toolchain` 行，构建不设 `GOTOOLCHAIN=local`）、`-trimpath`、`-buildvcs=false`、`CGO_ENABLED=0`、`GOFLAGS=` 清空、版本与提交经 `-ldflags -X` 注入（版本 = 发行版本，提交 = HEAD 完整哈希）、无 `lingedge_testdouble` 标签。
- `make release` 总会**同源再构建一次**：把源树复制到另一目录，使用**干净的 GOCACHE**（`-repro-fresh-cache`，缺省开）重新构建，逐个二进制比对 SHA-256，记录在 `repro.json`；任何不一致即失败。
- 发行必须来自干净提交（`-allow-dirty` 仅用于演练）。

### 验证记录

演练记录（真实 `release build`，4 个二进制 = server/tools × linux-amd64/arm64，第二次构建在另一目录 + 干净 GOCACHE）见 `docs/bmad/stories/1-34-发行流水线与离线签名流程-待签清单-包-sbom.md` 的「验证记录」一节；每次发行的 `repro.json` 随 `publish/` 归档。

## 8. OpenResty 发行目录与符号链接

OpenResty 发行目录里有大量符号链接（`libfoo.so -> libfoo.so.1`、`bin/openresty -> nginx`），而 Tools 的 `SafeExtract` **拒绝包内任何链接且不放宽**（tools-design §18.1、G-4）。流水线在**构建期**把链接消除：

- 文件链接 → 复制所指文件；目录链接 → 复制整棵子树；硬链接 → 独立的普通文件。
- 拒绝并使构建失败的情形：链接目标在源树之外、悬空、循环（指向祖先）、设备/FIFO/套接字、条目名不能通过 SafeExtract 同款路径规则（含仅大小写不同的重名、过深、百分号编码）。
- 被消除的链接逐条记录在 `build-record.json` 的 `links_resolved`，供发行负责人审阅；最终 payload 若残留任何链接，打包前即失败。
- 用法：`make release RELEASE_FLAGS='-openresty-version 1.27.1 -openresty-tree linux-amd64=/path/to/openresty -openresty-tree linux-arm64=/path/to/openresty-arm64'`。OpenResty 的来源方案与目录树属 Epic 3（UD-4），此处只提供经测试的消除机制。

## 9. 公钥轮换、吊销与泄露处置

**轮换**（tools-design §9.1，多公钥并存的重叠期）：

1. 离线机 `release-sign keygen -key-id rel-2027-a …` 生成新密钥对，得到新公钥条目 `new.keys.json`（`status: active`）。
2. 离线机用**旧 active 私钥**背书：`release-sign endorse -key <旧私钥> -new-keys new.keys.json -out rotation.json`。
3. 在线机发行时 `RELEASE_FINALIZE_FLAGS='-key-rotation rotation.json'`：清单携带 `key_rotation`，由旧 key 签名。Tools 验证背书通过后把新公钥落盘到 `/var/lib/lingedge/tools/trust/keys.d/`，此后新 key 签名的清单即可验证（端到端见 `TestKeyRotationEndToEnd`）。背书者必须是当前可信集中未吊销、active 的旧公钥；任一背书不成立则整份清单被拒绝。
4. 在新版 Tools 的内置 `keys.json` 中加入新公钥并把旧 key 改为 `retired`；重叠期结束后再从内置集移除。

**吊销**：
- 版本吊销：`RELEASE_FINALIZE_FLAGS='-revoke <组件>@<版本>=<原因>'` 写入清单 `revoked_versions`；命中则 Tools 拒绝安装/作为升级目标，已装则 `doctor` 告警。
- 密钥吊销：在新版 Tools 的内置 `keys.json` 的 `revoked_key_ids` 中加入该 key_id（清单内**不含** `revoked_key_ids`），随新版 Tools 发行；`keys.d` 内文件也可追加吊销。

**私钥泄露处置（建议流程，UD-3 待确认）**：① 立即停止使用该密钥签名；② 在离线机生成新密钥并用**仍可信的**旧 key（若旧 key 即泄露的 key，则需走新版 Tools 的人工分发渠道）完成轮换；③ 发布新版 Tools，内置集把泄露 key 加入 `revoked_key_ids`；④ 对泄露窗口内由该 key 签名的清单，用更高 `sequence` 的新清单取代，并通过 `revoked_versions` 吊销受影响版本；⑤ 通告用户升级 Tools（Tools 的更新本身用旧 active key 签名——泄露的是 active key 时存在自举问题，这是 ADR-012「可被推翻处」之一，需在 UD-3 确认应急渠道，如官网/Git 标签上的带外信任锚）。

## 10. 测试与门禁对照

| 验收点 | 对应测试/门禁 |
|---|---|
| 清单/包格式、META/SHA256SUMS、符号链接消除 | `tests/release`（`TestPublishLayoutAndManifestContent`、`TestOpenRestyTreeSymlinksEliminated`）、`scripts/release` 单测（`TestCopyTree*`、`TestWritePackageDeterministicAndOrdered`） |
| 离线签名、仓库/CI 无私钥（G-7 密钥扫描） | `scripts/release-sign` 测试、`gates secrets`（`make security`）及 `scripts/gates/secrets_test.go` |
| 测试密钥签名 → Tools 验签通过；篡改 → 拒绝（G-8） | `TestToolsVerifiesPublishedRelease`、`TestTamperingIsRejectedByTools`、`TestPipelineVerifyRejectsTampering`、`TestFinalizeRejectsBadFirstRoundSignatures` |
| §9.6 向量与 1.27 golden 交叉验证（Ruby OpenSSL × Go ed25519） | `scripts/release-sign`：`TestGolden96ReproducedBySigner`、`TestGolden96VerifiedByRubyOpenSSL`、`TestRubyOpenSSLSignsSameAsGo`；`tests/release`：`TestRubyOpenSSLVerifiesPipelineSignatures` |
| 可复现性 | `TestReproducibilityRecord`、`repro.json`、`TestCompareBuildsDetectsMismatch` |
| 生产构建无 `lingedge_testdouble`、含测试公钥的 keys.json 构建失败 | `TestProductionBuildFlags`、`TestReleaseBuildRejectsBadBuiltinKeys`、既有 `gates release-keys`/`tests/security/releasekeys` |
| `release.base_url` 可配置（T-3） | `make release-finalize RELEASE_BASE_URL=…`；Tools 侧 `release.base_url` 的清单地址与发行目录布局由 `TestPublishLayoutAndManifestContent` 对账 |

Ruby 交叉验证需要 OpenSSL ≥ 1.1.1 的 Ruby（macOS 自带 Ruby 2.6/LibreSSL 3.3 不支持 Ed25519；用 `RUBY=/opt/homebrew/bin/ruby` 指定）。缺失时测试明确跳过并标「等待环境验证」，`make release-sign-test` 缺省把缺失视为失败（`RELEASE_REQUIRE_RUBY=0` 可降级）。

## 11. 已知边界与待办（如实记录）

- **UD-3**：真实签名人、保管介质、轮换/吊销演练周期、公开发行源地址与应急渠道待用户确认；确认前本流程以测试密钥在本机演练通过为准，**不得对外发行**。
- **产物内置公钥的实际覆盖**：`cmd/lingedge` 目前只有 `--version` 脚手架，尚未链接 `internal/tools/manifest`，发行二进制里还没有 `go:embed` 的 `keys.json`；守卫（`gates release-keys`、产物字节的测试公钥扫描、`tests/security/releasekeys` 的真实 `go build` 扫描）已就绪，随 Tools 命令接线（Story 1.35）对真实 `lingedge` 产物生效。
- 在线取包路径（HTTPS 发行源）由 Story 1.28 的 downloader 负责；本流水线用离线 bundle 路径（同一条校验链）验证，并产出可直接发布的目录。
- 许可证扫描（G-7 子项）尚未选型接入；SBOM 的 npm 部分待前端工程（Story 1.36）出现 lockfile 后纳入。
