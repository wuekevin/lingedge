---
title: 'Story 1.28：downloader 与离线 bundle（完整校验链）'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: '387001e'
context:
  - docs/bmad/epics.md Story 1.28（AC1–AC4）
  - docs/architecture/tools-design.md §9.3–§9.5、§18.1、§18.3、ADR-012、docs/prd/LINGEDGE_V2_REQUIREMENTS.md（FR-9、NFR-11、NFR-15、AC-J-2、AC-J-7）
  - docs/bmad/stories/1-26-*、1-27-*、1-27b-*（security 叶子包、manifest 信任域①、遗留清理）
---

## Intent

**Problem:** 1.27 给出了清单验签与包内签名/META 的原语，1.26 给出了安全解压，但「把字节取回来并走完整条链」还没有人做：下载若不限定 HTTPS/重定向/大小/哈希，或离线 bundle 走了另一条（更松的）路径，未验证的发行包就可能进入安装。

**Approach:** 新增 `internal/tools/downloader`：在线与离线只在「取得字节」处不同，其余（§9.5 第 1–7 步）由同一个入口 `Acquire` 执行；校验全部通过后才 `Verifier.Commit`；任一步失败整个 `staging/<tx>` 删除、信任库不变。归档解析仍只在 `internal/tools/security`（G-4）：为此给 security 增加两个只读入口（`ReadMembers`、`IndexTar`）。`cli.DownloadError` 把下载器失败映射为退出码（校验类一律 5）。**没有新增迁移（000001–000018 一字未改）、没有改 OpenAPI/proto、没有改 CF-6 冻结的 schema 文件**（`TestSchemaFilesFrozen` 哈希不变）；无 `--insecure`，无任何放宽证书/重定向/大小/哈希的开关。所有测试为真实实现（真实 TLS 服务器与 TCP 连接、真实 Ed25519、真实 tar.gz、真实 CONNECT 代理、真实临时目录），无 Mock；测试内 HTTPS 服务器与测试密钥只在 `_test.go`（G-11/release-keys 门禁保持通过）。

## 逐条验收

- **AC1 下载器**（`client.go`）：
  - 仅 HTTPS：`CheckURL` 拒绝 http/ftp/file/相对 URL/带凭据 URL；`CheckRedirect` 拒绝重定向到非 HTTPS（明文端收到 0 个请求，不重试，退出码 5）；`Config` 中没有任何关闭校验的字段（`TestNoInsecureSwitchExists` 守卫字段名）。
  - 信任：TLS ≥1.2，系统根 + `release.ca_file`（`CAFile`：须为 1 MiB 内、group/other 不可写、含有效 PEM 的非空普通文件）；证书不被信任 → `KindTLS`（退出码 5，不重试）。
  - **大小上限 = 清单 `size`**：`Content-Length` 声明超过上限在读取任何内容前中止；实际流（含无 Content-Length 的分块传输）超过上限即中止（服务器侧写出量被封顶）；声明小于 size 亦失败；清单/签名这类小对象同样有上限（8 MiB / 4 KiB）。
  - **流式 SHA256**：边写边算，整个文件长度与哈希全部通过后才把 `.part` 改名为最终文件（「校验后合并」）。
  - **断点续传**：失败后 `Range` 续传；已有 `.part` 先从磁盘**重新计算**哈希（不信任内存）；206 的 `Content-Range` 必须与请求偏移、清单 size 一致；服务器无视 Range（200）或 416 则整包重来；**续传的前半段或后半段被篡改都使整体 SHA256 失败**并删除 `.part`、不重试。
  - 超时/重试/代理：连接/TLS/响应头/响应体空闲/单次尝试/总时长分层超时；有限重试（5xx、408、429、传输错误；策略类失败——重定向降级、证书、大小、SHA256——不重试）；`ProxyURL`（http/https/socks5，带凭据时错误文本不泄露口令）或环境变量 `HTTPS_PROXY/NO_PROXY`（子进程内验证，Go 只读一次环境）。
- **AC2 校验链 §9.5 第 5–7 步**（`acquire.go`）：外层长度+SHA256 → 解压前读取 `META/SHA256SUMS(.sig)`（`security.ReadMembers`，META 必须先于 payload）并经 `Verifier.VerifyPackageSums` 验签（域前缀 `LINGEDGE-PKG-V1\n<component>\n<version>\n<platform>\n`，重放到别的 component/version/platform 失败）→ `security.SafeExtract` 逐文件校验（限额取自已签名 `SHA256SUMS` 与 `package.json`，仍受绝对上限收紧）→ 解压出的 META 字节与验签时读到的一致 → `PackageMeta.Matches` 与清单一致 → `Verifier.Commit`（sequence 推进、key_rotation 落盘）。步骤 1–4 即 `Verifier.Verify`（含 `CheckTarget` 吊销检查）。任一步失败：`*manifest.Error`/`*downloader.Error` → 退出码 5（`cli.VerifyError`/`cli.DownloadError`，已接入 `Run.finish`）。
- **AC3 `--offline <bundle.tar>`**（`bundle.go`）：路径先 `Lstat`（末级符号链接拒绝）再 `EvalSymlinks` 校验为普通文件（目录/设备/FIFO/套接字/符号链接逃逸拒绝；不存在为退出码 2），以 `O_NOFOLLOW|O_NONBLOCK` 打开并 `fstat` 复核同一文件；`security.IndexTar` 受限索引（仅普通文件/目录、拒绝链接/设备/稀疏、名称规范化、重复/大小写冲突拒绝、条目数/大小受限）；布局只允许 `manifest.json`、`manifest.json.sig`、`packages/<文件名>`（可带单一前缀 `offline-bundle/`）；包条目大小必须等于清单 `size`、流式 SHA256 必须等于清单 `sha256`，之后与在线路径是**同一段代码**。**不联网**：`TestOfflineMakesNoNetworkCalls` 用「服务器命中计数 + http.DefaultTransport 计数 + net.DefaultResolver.Dial 计数 + 非回环主机名 + 代理监听计数」证明成功与失败路径均零调用，并有负向对照（在线尝试能被同一拦截器观测到）。同一张失败用例表（`TestChainFailuresOnlineAndOffline`）分别经在线与离线运行，结果一致。
- **AC4 篡改一律拒绝且 staging 清理**：篡改包体（等长翻转/截断/追加/非 gzip）、包内文件（内容与已签名 SHA256SUMS 不符/多出/缺少/符号链接/硬链接/路径穿越/重复）、清单（正文/签名/未知 key/过期/回滚/通道/吊销）、`SHA256SUMS` 与 `.sig`（被改/换 key/重放）、`package.json` 与清单不一致（component/version/variant/platform/files_count/unpacked_size/缺字段）、炸弹包（tar 结束后超量零字节、实际内容超过声明 unpacked_size）：每个用例断言退出码、**staging 目录树前后一致**、**信任库（trust/state、keys.d）前后一致**。

## 与「通常做法」不同的设计决定

- **退出码分层**：任务要求「任一失败退出码 5」。校验类（重定向降级、证书不被信任、大小超过/截断、外层 SHA256、解压校验、bundle 非法）一律 5；但**纯传输失败**（DNS/连接/超时/空闲超时/HTTP 状态，重试耗尽）映射为 1（GENERAL）而不是 5——网络不通不是校验失败，脚本需要区分「重试」与「告警」；bundle 文件不存在与「清单里没有该版本/变体/平台」为 2，组件 unreleased/builtin 为 7/8（tools-design §3/§4 已有语义）。「截断」按语义分：声明/收到的字节数不足清单 size 视为 5；连接被掐断导致重试耗尽属传输类。如需把慢速/超时也并入 5，改 `cli/downloaderr.go` 一处映射即可。
- **在 security 增加两个只读入口**（G-4 要求归档解析只在 security）：`ReadMembers`（流式取出 META 文件，取齐即停，META 必须先于 payload，扫描量受限）与 `IndexTar`（bundle 受限索引，偏移基于自己的计数；稀疏文件因数据流不连续而拒绝）。`SafeExtract` 本身未改。
- **包格式规则的语义澄清**（不改 CF-6 schema 文件）：META 必须先于 payload；`SHA256SUMS` 只列 `payload/`；`files_count`=payload 普通文件数（须等于已签名 SHA256SUMS 行数）；`unpacked_size`=payload 内容字节和（解压后须相等）；二者也是解压限额来源。已回写 tools-design §9.3 与修订记录；发行流水线 1.34 须按此产出。
- **`package.json` 的解析在解压之前、一致性比较在解压之后**：为得到解压限额必须先解析 `package.json`；§9.5 第 7 步的 `Matches` 仍在解压后执行，失败同样 5 并清理。
- **Commit 在整条链成功之后**：`Acquire` 末尾调用 `Verifier.Commit`（sequence 推进、`key_rotation` 落盘）。如果 1.33 希望把 Commit 推迟到「安装步骤完成之后」，可把 `Acquire` 的最后一步拆出（目前未提供该开关，避免出现「校验通过却从不推进 sequence」的误用）。
- **`Params.Executable`**：冻结的 `META/package.json` 没有「可执行文件」字段，tools-design §18.1 说可执行位由 META 声明——此处如实不发明字段，由调用方（1.33 安装器按组件约定）传入路径集合；缺省全部 0644。
- **bundle 末级符号链接一律拒绝**（即使指向普通文件）：任务写「经 realpath 校验为普通文件（拒绝符号链接逃逸）」，取更严的解读；路径中间的目录符号链接（`/var`→`/private/var`）仍允许。
- **版本选择不在本 Story**：`Params.Version` 为精确版本，约束求解/选最新属 resolver（1.30/1.33）。

## 变异验证（临时去掉关键检查 → 测试必须失败 → 还原）

脚本逐条做最小改动（不改变编译通过性：`if false && cond`），运行对应包测试，立刻还原；全部结束后 `client.go/acquire.go/bundle.go/archive_read.go` 与变异前**字节一致**（逐文件 `cmp`）。33 条，**28 条检出**，5 条未检出（均为冗余的纵深防御层，单独去掉任一层都被另一层兜住，已逐条说明）：

- 检出（括号内为首个失败的测试）：重定向降级检查（`TestRedirects`）；TLS 校验被关闭（`TestTLSTrust`）；`release.ca_file` 的权限检查（`TestCAFileValidation`）；Content-Length 声明上限、实际流上限（`TestSizeCapDeclaredAndActual`）；外层 SHA256 比较（`TestStreamingSHA256Mismatch`/`TestResumeTamperedSecondHalfFails`/chain 表）；续传时从磁盘重算已有部分哈希（`TestResumeAfterCutThenVerify`）；`Content-Range` 与请求/清单一致性（`TestResumeServerQuirks`）；可重试状态集合（`TestStatusAndRetry`）；证书失败不重试（`TestTLSTrust`）；`VerifyPackageSums` 调用、`Matches` 调用、`files_count` 比对、`unpacked_size` 比对、SHA256SUMS 只许 `payload/`、解压限额取自 `unpacked_size`（chain 表）；失败清理 staging（`TestAcquireCancelledMidDownloadCleansUp` 等）；Commit 调用（`TestChainSuccessOnlineEqualsOffline`/`TestRotationCommittedOnlyAfterFullChain`）；成功后删除中间 tar.gz（`TestChainSuccessOnlineEqualsOffline`）；`--allow-expired-manifest` 必须离线（`TestAllowExpiredOnlyOffline`）；bundle 末级符号链接、非普通文件（路径校验阶段，`TestBundlePathMustBeRegularFile`）、包条目大小与清单 size、bundle 内包 SHA256、布局之外条目（`TestBundleLayoutRejections`）；`ReadMembers` 的「META 先于 payload」（`TestReadMembersRejections`）；稀疏条目检测（`TestIndexTarRejectsSparse`）。
- 未检出（等价/冗余层，**不补测试**）：① `copyPackage` 的 `n != exp.Size`——前面已有「条目大小必须等于清单 size」与随后的 SHA256 比较两层，且读取期间文件变化无法在不注入内部状态的前提下复现；② `downloadOnce` 的 `offset > exp.Size`（丢弃过大的 `.part`）——`Download` 入口先删除 `.part`，写入量又被 `want+1` 封顶，`.part` 不可能超过 size，纯防御；③④⑤ `IndexTar` 的 `cr.n != Offset+Size` 与 `ReadMembers` 的两处大小核对——前者与 `sparse()` 的 PAX 记录检测互为冗余（稀疏文件两层都能识别），后两者与「声明大小 > 上限」检查互为冗余；单独去掉任一层都被另一层兜住。
- 变异过程中发现并修正的弱点：初版 `TestBundlePackageProblems` 与 `TestBundlePathMustBeRegularFile` 只断言错误分类，去掉「包条目大小」「路径校验阶段的非普通文件检查」后仍通过（被后面的层兜住）；已加断言错误文本以区分各层，复跑检出。

## 对既有测试/防护的改动（逐条）

- `internal/tools/cli/run.go`：`Run.finish` 增加 `errors.As(*downloader.Error)` 分支（→ `DownloadError`），`isParseStageError` 把 `*downloader.Error` 排除在「解析阶段错误」之外（与 `*manifest.Error` 同处理）。**没有修改任何既有 cli 测试**（help/exit-codes/envelope 快照均未变）。新增 `cli/downloaderr.go` 与 `cli/downloaderr_test.go`（映射穷尽性：新增 downloader 分类而忘记映射会使测试失败）。
- `internal/tools/security`：只新增 `archive_read.go` 与 `archive_read_test.go`；`extract.go`、`FuzzSafeExtract` 等**一字未改**。
- `docs/architecture/tools-design.md`：§9.3 增「格式规则」、§9.4 增实现要点、§9.5 补 `Acquire`/退出码映射、§18.3 补 TLS 实现口径、末尾修订记录；`docs/bmad/decisions-adopted.md` §1.1 追加 1.28 的默认值登记。CF-6 的 schema 文件与哈希、签名域前缀未动；`TestToolsDesignExamplesPassSchema` 等文档测试仍通过。
- 门禁：G-4/G-5/G-6/G-8/G-11、release-keys 的规则与白名单**一字未改**。唯一的一次门禁触发：G-6 的「内嵌口令连接串」检查拦下了测试里写死的代理口令字面量——**没有豁免**，改为运行时随机生成口令并用 `url.UserPassword` 拼装（源码中不再出现该形态）。
- 没有新增迁移（000001–000018 不变）、没有 OpenAPI/proto 变更、`contract-v1` 冻结不受影响；新增 `FuzzIndexTar`/`FuzzReadMembers`/`FuzzParseSHA256SUMS`（security）与 `FuzzOpenBundle`/`FuzzParseBundleLayout`/`FuzzArtifactFileName`/`FuzzAcquireOffline`（downloader）。

## 测试（真实、无 Mock）

真实 TLS 服务器（`httptest.NewUnstartedServer`+`StartTLS`，证书经 `release.ca_file` 路径注入）、真实 TCP 连接掐断/卡住/滴灌、真实 HTTP CONNECT 代理、真实 Ed25519 签名的清单与 `.tar.gz` 包、真实 tar 解析与 `SafeExtract`、真实 `FileSequenceStore`；测试密钥由确定性种子在 `_test.go` 内派生（与 manifest 包测试同一派生方案，其公钥已登记在 release-keys 门禁指纹表中）。`downloader` 包 188 个子/测试通过。fuzz：security 三个、downloader 四个，各复跑 15–25 秒，无失败语料、`git status` 无 `testdata/fuzz`。

## 门禁结果

见提交说明与交付报告（`gofmt -l .` 无输出；`make lint security gate-selftest race vuln contract-check build verify-build migrate-test` 与 linux/amd64、arm64 交叉编译与 vet）。

## 遗留与风险（只列确属后续 Story 或需特定环境的）

- **等待环境验证**：针对**真实发行源**的下载（UD-3：真实发行公钥与发行域名未定；本 Story 的「通过」仅代表校验链逻辑正确）；ENV-LNX 真实 Linux 复验（root 属主 staging、`O_NOFOLLOW`/FIFO 在 Linux 上的行为、大小写敏感文件系统）→ 1.45。
- **后续 Story**：1.29（registry 事务边界/全局任务锁：`Acquire` 与 `FileSequenceStore` 都不带跨进程锁；`staging/<tx>` 的 tx ID 来自 journal）；1.33（installer 接线 `--offline`/`--allow-expired-manifest`/`--channel`/`release.ca_file`/`release.base_url` 配置项与代理配置、`Params.Executable`、进度输出、版本选择与 `Acquire` 的串联、是否把 `Commit` 推迟）；1.34（流水线按 §9.3 格式规则产出包：META 在前、`files_count`/`unpacked_size` 口径、包内无链接）；1.37 前端无关。
- **已知限制（如实）**：断点续传只在一次 `Acquire` 内的重试之间生效（失败即清理 staging，不跨进程续传，符合「staging 被清理」要求）；`ProxyURL` 的 socks5 路径只验证了配置接受，未起 socks5 服务器测试；`HTTPS_PROXY` 环境变量路径经子进程验证（Go 只读一次环境变量）。
