---
title: 'Story 1.27：manifest 与发行信任域①：验签 / sequence / 吊销 / 密钥轮换'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: 'd896487'
context:
  - docs/bmad/epics.md Story 1.27（AC1–AC7，含 2026-11-16 修订）与 CF-6
  - docs/architecture/tools-design.md §9.1–9.2、§9.3、§9.5、§9.6（真实示例向量）、§10（semver 约束语法）
  - docs/architecture/adr/ADR-012（发行信任域①）、docs/bmad/review-architecture*.md（IMP-18、MIN-04）
  - docs/bmad/stories/1-23-*、1-25-*、1-26-*（信任域②③、CLI 退出码、security 叶子包）
---

## Intent

**Problem:** Tools 要靠一份签名清单驱动安装与升级。若验签、防回滚、防冻结、吊销与密钥轮换分散在各命令里，被篡改/回滚/过期的清单就可能驱动安装；退出码 5（VERIFY_FAILED）也还没有真实的生产者；生产构建若内置了测试公钥，等于把「谁都能签发合法发行清单」带进产物。

**Approach:** 在 `internal/tools/manifest` 实现信任域①的全部校验逻辑（只验签，不含任何签名/私钥代码；不 import `pkg/signing`，域前缀互不相同），`cli.VerifyError` 把 `*manifest.Error` 统一映射为退出码 5（信任库损坏为 3、参数错误为 2），`gates release-keys` 让生产构建「缺失内置公钥或含测试公钥即构建失败」，并按 CF-6 冻结 manifest/keys/sig/包 META 的 schema 与三个签名域前缀。

## 逐条验收

- **AC1 验签与 VERIFY_FAILED**（`keys.go`、`verify.go`、`builtin.go`）：`keys.json`（`go:embed all:builtin`，多公钥 `active|retired|revoked` + `revoked_key_ids`，严格解析）与 `trust/keys.d/*.json` 合并为 `TrustSet`（同名异钥拒绝加载、吊销集取并集、至少一把未吊销公钥）。`Verifier.Verify` 对 `"LINGEDGE-MANIFEST-V1\n"` ‖ `manifest.json` **原始文件字节**验 Ed25519：篡改清单（逐字节翻转每一位置、追加空白）、篡改/格式非法/算法不符/多余字段的 `.sig`、裸签名（无域前缀）、冒用他人 key_id、未知 `key_id`、`revoked_key_ids` 命中（即使 status 写 active）、`status=revoked`、签名文件所用 key 的 status/有效期窗口不允许——一律返回 `*manifest.Error`，经 `cli.VerifyError` 得到**退出码 5**（`TestVerifyFailuresProduceExitCode5` 用真实 Verifier 的真实错误走 `Run.finish`，断言 envelope `exit_code=5`、稳定错误码、符合 `envelope.schema.json`）。
- **AC2 sequence / 过期 / 吊销**：`FileSequenceStore`（`trust/state`，JSON，0600，原子写，**按通道**记录，只升不降）；`sequence < 已见` → `sequence_rollback`，相等允许（幂等重取）；`state` 损坏/未知字段/负数/未知通道/权限过宽一律失败关闭（退出码 3），**不当作「从未见过」**（否则删改该文件即可旁路回滚防护）；`Verify` 本身不写任何状态，整条校验链成功后才 `Commit`（先落盘轮换公钥再推进 sequence，二者幂等）。`valid_until`：`now >= valid_until` 即过期（边界测试 前一纳秒通过/恰等于拒绝），默认拒绝；`--offline` + `--allow-expired-manifest` 放行并产生告警，**放行过期不放行回滚/篡改**；`--allow-expired-manifest` 不带 `--offline` 在 manifest 层（`KindOptions`）与 CLI 层（`install`/`update` 的 `usage.flag_requires`，退出码 2）都被拒绝。`revoked_versions`：`Manifest.CheckTarget` 命中（按 SemVer 优先级，忽略构建元数据）→ `version_revoked`（退出码 5）。
- **AC3 key_rotation**：清单顶层 `key_rotation:{keys[],endorsements[{key_id,endorsed_by,sig}]}`；仅在清单本身验签通过后处理；背书者必须是当前可信集中**未吊销、active、generated_at 在其窗口内**的旧公钥；背书签名输入 `"LINGEDGE-KEYROT-V1\n"` ‖ 新公钥全部语义字段（背书后改有效期/状态即失败）；新公钥必须 active、不得与已有公钥同名异钥、不得复活已吊销 key_id；任一条不成立整份清单被拒绝。`Verifier.Commit` 把新公钥原子落盘到 `trust/keys.d/<key_id>.json`（文件 0600、目录 0700）并并入内存信任集；重启后 `LoadTrust` 重新读到；重叠期新旧公钥并存（新旧都能验证）；`keys.d` 里的符号链接、group/other 可写、损坏文件、与内置同名异钥都使加载失败。
- **AC4 schema 与 semver**（`manifest.go`、`version.go`）：未知可选字段（五个层级）忽略；「必需」由对象内 `critical` 数组声明，解析方不认识其中任一名字即整份拒绝（五个层级逐一测试）；`schema_version≠1`、必需字段缺失（含 `revoked_versions`）、枚举/格式/范围非法（channel、sequence、时间、组件状态、版本号、platform、https url、sha256、size、端口、约束语法……60+ 项拒绝用例）、重复 JSON 键、超深嵌套、超 8 MiB 一律拒绝；§9.2 示例形态的完整清单（补齐占位）可解析。semver 固定 `github.com/Masterminds/semver/v3 v3.5.0`：版本用 `StrictNewVersion`（拒绝前导 v、前导零、缺段），约束限定为 tools-design §10 的语法子集并逐 token 预校验（通配/`!=`/连字符范围/逗号拒绝）；边界测试覆盖 SemVer §11 优先级链、`>= > <= < = ^ ~ ||`、`^0.x`/`^0.0.x`、部分版本补零、预发布（约束不含预发布段时预发布版本不满足）与构建元数据；三个 fuzz（约束、版本、清单、签名文件）。
- **AC5 构建策略**（`releasekeys.go`、`Makefile`、`.gitignore`）：内置公钥集由发行流水线注入 `internal/tools/manifest/builtin/keys.json`（不入库；`builtin/README.txt` 说明）。`gates release-keys`：含测试公钥（与 `data/test-release-pubkeys.txt` 指纹相同，字节比较与编码无关；README 等任何文件里的 base64/hex 也算）、key_id 含 test/golden/demo 等字样、含私钥/种子字段、`builtin/` 下杂散文件（会被 go:embed 打进产物）、无 active 公钥 → **任何构建失败**；缺失 → 发行构建（`make build RELEASE=1`）失败，开发构建放行（产物内置零公钥，运行期对任何清单 `no_builtin_keys` 失败关闭，退出码 5）。`make build` 前置守卫、`make security` 运行守卫。G-11 testdouble 门禁额外把测试公钥（base64/hex）当作固定假数据扫描生产源码与产物。测试密钥/公钥只存在于 `_test.go`、`testdata/`、`tests/`（均由确定性种子派生）；**未新增任何带 `lingedge_testdouble` 标签的文件**。真实公钥待发行签名人确认（UD-3）。
- **AC6 冻结点 CF-6**（`schema/*.json`、`docs/bmad/contract-changes/CF-6.md`）：manifest/keys/sig/package-meta 四份 JSON Schema（`go:embed`），由 `TestSchemaFilesFrozen` 以 SHA-256 钉住，另有「真实向量通过 schema、非法变体被 schema 与 Go 双重拒绝」的一致性测试；三个签名域前缀常量被 `isolation_test.go` 钉住。冻结记录与对 tools-design §9 的补全逐项登记在 CF-6.md，并已回写 tools-design §3/§9。
- **AC7 签名输入与 §9.6 golden 向量**（`testdata/vectors.json`、`golden_test.go`、`pkg.go`）：清单对**文件原始字节**签名；吊销拆为 `keys.json.revoked_key_ids`（含 `keys.d`）与清单 `revoked_versions`（清单内无 `revoked_key_ids`）；包为 `.tar.gz`，包签名对象 `LINGEDGE-PKG-V1\n<component>\n<version>\n<platform>\n` ‖ `SHA256SUMS`（`VerifyPackageSums`；改 component/version/platform 任一字段即失败；字段含换行在构造输入前被拒绝；清单签名不能充当包签名）；`META/package.json` 解析与 `Matches`（§9.5 第 7 步）。§9.6 向量原样落盘：种子→公钥→清单/包签名由 Go `crypto/ed25519` 确定性复算且与文档逐字节一致，生产验签路径通过，清单/签名/SHA256SUMS 每个字节翻转均失败，`sequence` 回滚/未知 key/吊销 key/过期均按预期失败。

## 与「通常做法」不同的设计决定

- **构建守卫分两档**：任务要求「缺失 keys.json 即构建失败」，但 UD-3 前仓库里没有任何真实公钥，若无条件失败则 `make build`/`go vet ./...`/`go test ./...` 全部无法运行。因此：缺失在**发行构建**（`RELEASE=1`）失败，在开发/CI 构建放行但产物内置零公钥并在运行期失败关闭；**含测试公钥在任何构建都失败**。`make release` 本体仍是 `NOT_IMPLEMENTED(1.34)`（既有 scaffold 测试断言该文案，我未改它），1.34 须经 `make build RELEASE=1`。
- **「未知必需字段」用 `critical` 数组定义**（tools-design 原文未给出可判定的定义），见 CF-6 §2。
- **sequence 按通道记录**、**公钥窗口以 `generated_at` 判定**、**`key_rotation` 与第三个域前缀 `LINGEDGE-KEYROT-V1`**、**schema 收紧点**：均为对 §9 的补全，已回写文档并登记在 CF-6。
- **`Env.BuiltinKeys`/`Env.TrustDir`/`Env.NewVerifier`**：给后续命令（1.33/1.43）的真实装配点；`DefaultEnv()` 引用 `manifest.BuiltinKeys`，使内置公钥路径在 `lingedge` 二进制里可达（`tests/security/releasekeys` 用 `go build -overlay` 证明注入的公钥确实被嵌入）。
- **`lingedge` 此前没有任何实现的命令会真正调用验签**（install/update 仍是 Hidden 的「尚未实现」）：退出码 5 的真实生产者是 `cli.VerifyError` + `Run.finish` 的 `errors.As` 分支；命令层接线随 1.33/1.43。没有为了「演示」新增命令（那会改动 CF-5 冻结的命令树）。
- **Ruby 第二实现**：§9.6 声称的 Ruby OpenSSL 验签在本机无法复验（Ruby 2.6 + LibreSSL 3.3.6 不支持 Ed25519）；如实登记为遗留，Go 侧独立复算已通过。

## 变异验证（临时去掉关键检查 → 测试必须失败 → 还原）

脚本逐条做最小改动、运行对应包的测试、立刻还原并核对文件内容一致（共 48 条 + 3 条补充）。**首轮 48 条中检出 44 条，4 条未检出，逐条处理如下**：
1. 「不检查 key 状态」（Verify 里的 status 判定）——`NewTrustSet` 已把 `status=revoked` 并入吊销集，该判定是纵深防御的第二层，首轮不可达。**已补测试** `TestVerifyRejectsKeyWithNonVerifiableStatus`（直接构造异常信任集），复跑**检出**。
2. 「allow-expired 不要求 offline」（`!Offline || !AllowExpired` 里的 `!Offline`）——前面的 `KindOptions` 检查已使该分支不可达，属**等价变异**（两层同一约束），未新增测试。
3. 「revoked_versions 缺失放行」（`requireFields` 中去掉该字段）——后面的 `RevokedVersions == nil` 检查同样拒绝，属**等价变异**（两层同一约束；「缺 revoked_versions」「revoked_versions 为 null」两个用例仍分别断言拒绝）。
4. 「门禁不查测试公钥」（`checkKeysJSON` 的逐 key 比较）——同一文件里的 base64/hex 内容扫描是第二层，单独去掉任一层都被另一层兜住；补充变异「**两层同时去掉**」与「仅去掉内容扫描」均**检出**。
首轮检出的 44 条覆盖：去掉验签、清单签名不含域前缀、未知/吊销 key 放行、公钥窗口、sequence 回滚/相等边界、过期/过期边界/告警、allow-expired 无 offline、通道不符、revoked_versions 失效、轮换（背书签名不验/retired 背书/同名替换/复活吊销/缺背书/整体不处理）、critical 放行、重复键、schema_version、sha256 格式、https、损坏 state 当空、sequence 可回退、包签名不绑定 component/不查窗口、semver 宽松解析、约束子集、keys.d 同名异钥/符号链接/宽权限、alg、cli 退出码映射（两处）、cli allow-expired、门禁各规则（发行缺失/杂散文件/私钥字段/key_id 字样）、Makefile 前置守卫与 `-require`、testdouble 不扫测试公钥。另手工验证：从指纹表去掉一把测试公钥后 manifest 与 cli 的单测会失败（登记检查生效）。
变异验证中发现并修正的两处弱点：① 首轮未检出的第 1 项（上文）；② 门禁的两层冗余检测会互相掩盖单点退化，已用「两层同时去掉」的变异证明联合有效。

## 对既有测试/防护的改动（逐条）

- `scripts/gates/main.go`：`gates` 表**增加** `release-keys`，`options` 增加 `requireKeys`，增加 `-require` 标志（只增不减）。
- `scripts/gates/testdouble.go`：G-11 的假数据标记集合**扩充**为 `fake-data-markers.txt` ∪ 测试发行公钥（base64/hex）——只收紧不放宽；`scripts/gates/data/test-release-pubkeys.txt` 为新增数据文件。
- `tests/gates/gates_selftest_test.go`：干净副本门禁清单**增加** `release-keys`；**新增** 2 个负向用例（含测试公钥、杂散文件）与 `TestReleaseKeysGateRequireMode`；原有用例未改。
- `Makefile`：新增 `release-keys-check` 目标；`build` 增加前置守卫（先于 `rm -rf dist`）；`security` 增加 `$(GATES) release-keys` 与文案；`release` 目标**保持** `NOT_IMPLEMENTED(1.34)`（仅加注释）；`.PHONY` 增项。
- `.gitignore`：增加 `internal/tools/manifest/builtin/keys.json`。
- `internal/tools/cli/run.go`：`Run.finish` 的错误映射**增加**一个 `errors.As(*manifest.Error)` 分支（置于 `context.Canceled` 之前、`*Error` 之后）；`commands.go`：`install`/`update` **增加** `--allow-expired-manifest` 标志与「须与 `--offline` 联用」的用法校验；`env.go`：`Env` 增加 `TrustDir`/`BuiltinKeys` 字段与缺省值。**没有修改任何 cli 既有测试**（help/exit-codes/envelope 快照均未变）。
- `go.mod`/`go.sum`：新增依赖 `github.com/Masterminds/semver/v3 v3.5.0`（`make vuln` 无漏洞）。
- `docs/architecture/tools-design.md`：§3 增 `--allow-expired-manifest`，§9.1/9.2/9.5/9.6 回写（见 CF-6.md §2）。
- **未**改动：`.golangci.yml`、迁移 000001–000018、`pkg/signing`、`scripts/gates/trustdomain.go`（Server 代码路径的发行私钥/`LINGEDGE-MANIFEST-V1` 字面量禁令对 `internal/tools/**` 本就豁免，门禁与 `tests/security/trustdomain` 二进制符号层检查照常通过）、G-5/G-6/G-8 规则。**没有放宽或豁免任何门禁**：我的测试曾被 G-6 命中（测试里写了带凭据的 URL 字面量），已改为运行期拼接，未加任何豁免。

## 测试（真实、无 Mock）

真实 Ed25519 签名/验签、真实临时目录里的 `keys.d`/`trust/state`（含权限位、符号链接、损坏文件）、真实 `Run.finish`/envelope/JSON Schema、真实 `make`/`go build -overlay`/`go run ./scripts/gates`；没有任何对验证逻辑的桩或 mock。测试私钥由确定性种子在 `_test.go` 内派生，公钥登记在 `test-release-pubkeys.txt`（manifest 包的 `TestMain` 与 cli 单测机器核对）。唯一的「注入」是 `SequenceStore`/`Env.BuiltinKeys` 这类依赖边界（内存实现只用于不关心落盘的用例，落盘行为由 `FileSequenceStore` 用例覆盖）。

## 门禁结果

提交前在本机（macOS arm64，go1.27.4；本 Story 的测试不需要 MySQL/Redis，`make migrate-test`/`make race` 内的集成测试由 tests/mysqltest、tests/redistest 自启临时实例并清理，未触碰常驻 mysqld 与 127.0.0.1:6390 的 redis）同步执行：`gofmt -l .` 无输出；`make lint`（0 issues）、`make security`（含新增 `release-keys`）、`make gate-selftest`、`make vuln`（No vulnerabilities found）、`make contract-check`、`make build`、`make verify-build`、`make migrate-test` 退出码均为 0；`make race`（`-race`，转后台运行并等待至结束后核对：65 个包 ok、无 DATA RACE/FAIL/panic，退出码 0）。`GOOS=linux GOARCH=amd64|arm64` 的 `go build ./...` 与对 `internal/tools`、`tests/security`、`scripts/gates` 的 `go vet` 通过，`GOOS=windows` 可编译 manifest 包。限时模糊：`FuzzParseConstraint`/`FuzzParseVersion`/`FuzzParseManifest`/`FuzzParseSig` 各 20s，无失败、无新增失败语料。

## 遗留与风险（只列确属后续 Story 或需特定环境的）

- **UD-3（真实发行公钥）**：仓库内没有任何发行公钥；真实发行与真实安装验收被阻塞（不阻塞开发）。生产构建的 `builtin/keys.json` 由 1.34 流水线注入。
- **ENV-LNX**：`/var/lib/lingedge/tools/trust` 的 root 属主与 0700/0600 语义（本机仅验证权限位与符号链接/宽权限拒绝，单用户非 root）——**等待环境验证**。
- **第二实现交叉验证**：Ruby OpenSSL 对 §9.6 向量的复验（本机 Ruby 2.6/LibreSSL 不支持 Ed25519）——随 1.34（流水线签名器产出同口径向量）进行。
- **后续 Story（接线与消费）**：1.28 downloader 调 `Verifier`/`VerifyPackageSums`/`PackageMeta.Matches` 与 `security.SafeExtract`（步骤 5–6 的流式 SHA256 与解压）；1.29 registry 对 `trust/state`/`keys.d` 的事务边界（本 Story 提供文件实现与「验签成功后才写」的 `Commit` 语义）；1.30 resolver 消费 `Manifest`/`Constraint`；1.33/1.43 命令经 `Env.NewVerifier` 接线并在整条链成功后 `Commit`；1.34 离线签名器与 `make release`（须经 `make build RELEASE=1`）。
- **已知限制（如实）**：`FileSequenceStore` 不自带跨进程锁（由 1.29 的全局任务锁保证）；清单 `requires.*` 约束值在 schema 阶段按语法子集校验但 `runtime` 里的非约束占位文本（§9.2 示例里的「示意」字样）会被拒绝——真实清单必须填约束；`key_rotation` 不支持多级链式背书（旧 active → 新，新公钥在下一次清单里才可作背书者）。
