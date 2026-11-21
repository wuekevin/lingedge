---
title: 'Story 1.30：resolver——依赖 / 版本 / 资源 / 端口求解（纯函数）'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: '66ab95b'
context:
  - docs/bmad/epics.md Story 1.30（AC1–AC5）
  - docs/architecture/tools-design.md §10、§11、§4、§8、§15、docs/bmad/decisions-adopted.md（D-16）、docs/prd/LINGEDGE_V2_REQUIREMENTS.md（FR-3、FR-5、FR-13）
  - docs/bmad/stories/1-27-*、1-29-*（manifest 的 Manifest/Constraint/Version、registry 的 State、cli 的 VerifyError/RegistryError 做法）
---

## Intent

**Problem:** 安装前没有可解释的 Plan：用户不知道会装哪个版本、占哪些端口、要多少内存与磁盘、会不会撞上本机已有监听，也没有一个地方统一判定 `scheduler|worker`（内置）与 `unreleased`（如 `install dns`）。1.25 的命令树只有 `cli.not_implemented` 占位，1.27 的 manifest 与 1.29 的 registry 提供了版本/约束与已装状态，但没人消费它们。

**Approach:** 新增 `internal/tools/resolver`（依赖图 resolver → manifest、registry；不依赖 cli、downloader、security），一个**纯函数** `Resolve(Input) *Plan`：无 IO、无时钟、无随机、不读环境。失败不返回 error 而是写入 `Plan.Blockers`（`--dry-run` 同样展示），`Plan.Err()` 给出 `*resolver.Error`，`cli.ResolveError` 把它映射到退出码并接进 `Run.finish`（与 VerifyError/DownloadError/RegistryError 同做法）。**没有新增迁移（000001–000018 不变）、没有 OpenAPI/proto 变更、没有改 CF-6 冻结的 schema、没有新增依赖。** 不接线 1.33 的编排：`install/update` 仍是未实现命令（`cli.not_implemented`），本 Story 只交付库与 cli 映射。系统探测的真实产出属 1.31，resolver 只消费 `System` 输入（结构见 tools-design §10.5，即 1.31 的产出契约）。

## 逐条验收

- **AC1 Plan 与 blockers**：`Plan{components,order,steps,ports,units,files,warnings,blockers}`（外加 `resources`）每个字段都有实质内容并被测试断言（`TestMergedPlanServerAdminWebWebsite`：组件版本/动作/下载信息、order、端口含条件端口 9444、units 路径与运行用户、files 的路径/权限/属主/策略、steps seq 连续、资源统计）。`blockers` 非空 → `Executable()==false`、`Err()!=nil`；有阻断时 Plan 仍完整展示组件/steps/ports/units/files（`TestBlockersPreventExecutionAndAreShownInDryRun`）；cli 侧阻断与完整 Plan 放进 `errors[].details`。
- **AC2 BUILTIN / NOT_RELEASED**：`scheduler|worker`（清单里是 `builtin` 或根本没有这个名字）→ BUILTIN（8，消息「已内置于 server」）；`status: unreleased` → NOT_RELEASED（7，不伪造成功）；`server+dns` 也整体阻断；两者并存时 7 优先（`TestBuiltinAndNotReleased`，含经真实 `Run.finish` 的 envelope 断言）。
- **AC3 依赖/版本/约束**：内部依赖闭包（node→openresty，order 中 openresty 先于 node；清单缺内部依赖则无解）；`requires.components`/`peer`/`external`/`runtime`（探测结果作为输入，Node.js `>=20 <23` 对 18/23/缺失均无解，对 20 通过）；**约束传播与完备回溯**（openresty 只有 1.25 时 node 回退 0.1.0；peer 迫使 server 取 0.1.0；外部依赖不满足时回退旧版本）；无解 → 退出码 3 并给**冲突链**（「node（用户选择）→ openresty（被 node@0.1.0 依赖）✗ openresty 没有满足全部约束的版本：…逐候选的拒绝原因」）；`min_upgrade_from`（不固定版本选可直达最高版并给逐级路径警告；固定到不可直达版本 3 并带 `upgrade_path`；降级 3）；已装组件**反向约束**与**被动升级**（标记 `passive_upgrade`，未确认 → 退出码 9，`AcceptPassiveUpgrade` 后放行并记警告）；`revoked_versions` 命中不可作目标（不固定版本时跳过吊销版本；固定吊销版本 → 5，复用 `manifest.CheckTarget`；已装版本被吊销可保持但警告）。预发布版本仅固定版本可选。
- **AC4 资源与端口**：内存合计、磁盘**按目标挂载点分别统计**（release 目录与 staging 各记在各自的最长前缀挂载点，`/opt` 与 `/var` 分开不足、同挂载点合计才不足、恰好够用的边界、只读/未覆盖的挂载点都有测试）；端口矩阵含组件间冲突、与本机监听冲突（`0.0.0.0` 对 `127.0.0.1`、反向、同地址、不同 loopback 不冲突、IPv6 `::`/`::1`、双栈、IPv4 映射、tcp6/udp6 归一、协议/端口不同），冲突在预检阶段被阻止并给出占用进程/pid/用户，宝塔环境 80/443 给 R-10 处置建议；已装组件自己的监听不算冲突；`--set` 换端口（`PortOverrides`）；资源不足/冲突都是 Plan 阻断（没有任何副作用发生）。
- **AC5 同机合并**：server + admin-web + website(static) 同机可解且合并成一个 Plan（server 先于 admin-web；website static 不占 3000，ssr 才占且带 `revalidate.secret` 与 `lingedge-website.service`）。
- **拓扑与环**：`order` 为依赖在前、同层字典序（Kahn）；成环给出环路径（`alpha → beta → gamma → alpha`）与退出码 3，不 panic、不死循环；自依赖被忽略。

## `System` 输入契约（1.31 直接对接）

定义在 `internal/tools/resolver/types.go`，文档见 tools-design §10.5 的表：`os/arch`、`memory_total_mb/memory_available_mb`、`disks[]{mount,fs_type,total_mb,free_mb,read_only}`、`listeners[]{proto,addr,port,pid,process,user,unit}`（`unit` 来自 cgroup，用于识别已装组件自己的监听）、`deps{mysql,redis,nodejs}{detected,version,source,flavor}`、`baota{detected,confidence,version,panel_port,nginx_path}`。字段全带 JSON 标签，1.31 的探测结果可直接 marshal 成同一形状；版本串保持原样（`8.0.36-0ubuntu0.22.04.1`、`v20.11.0`），数字前缀的归一在 resolver 内完成并有测试。

## 与「通常做法」不同的设计决定

- **失败进 Plan 而非 error**：`--dry-run` 与交互预检都要「展示」阻断，所以 `Resolve` 永远返回 Plan；`Plan.Err()` 才是退出码通道。多个阻断并存时按固定优先级（用法 2 > 未发布 7 > 内置 8 > 吊销 5 > 无解 3 > 成环 3 > 升级路径 3 > 系统 3 > 资源 3 > 端口 4 > 需确认 9）取退出码，全部阻断放进 details。
- **`requires.components` 的语义**：§10.2 说它约束「同机已装或本次一并安装」的组件，§10.1 说前端对 Server 只是配置层依赖。落地为：目标是内部组件 → 自动加入闭包；目标在场（已装/一并安装）→ 强制；非内部且不在场 → 警告 `dependency.remote_assumed`（按远端部署）；清单里根本没有 → 无解。`requires.peer` 同理：在场则强制并参与回溯，不在场只警告 `peer.compare_at_enroll`。
- **已装组件始终参与约束**（不止被依赖者）：否则升级 server 会悄悄破坏已装 website 的约束。代价是「用户要升级 server 而已装 website 钉住旧 server」时，选最高 server 并要求确认被动升级 website（与 §10.3「取最高满足者」一致），而不是静默保持旧版。
- **外部依赖作为候选过滤而不是事后检查**：这样 mysql 7 对 server 0.2、redis 8 对「要求 redis ≥9 的版本」会回退到可用的旧版本，全部被拒才是无解。未探测到时只有 `server` 的 mysql/redis 放行（§10.3 步骤 5），写在 `profile.go` 的 `remoteOK`，其余（含 `runtime.node`）一律拒绝。
- **磁盘记账**：manifest 只有一个 `min_disk_mb`，没有按目录拆分。记在 `<opt>/<组件>` 所在挂载点；另把包大小（向上取整 MiB）记在 downloader 的 staging 所在挂载点——这是实际会落盘的两处。数据目录不另计（manifest 没有该数据）。
- **端口通配语义保守**：`bind` 缺省视为 `0.0.0.0`；`::` 视为与一切冲突（Linux 双栈）；`0.0.0.0` 与 IPv6 具体地址不冲突。`System.Listeners` 没有 `IPV6_V6ONLY` 信息（`ss` 不给），因此 `0.0.0.0` 对 `::` 的冲突按双栈处理。
- **website 的 3000 端口只属 `ssr` 变体**：清单的 `ports` 在版本级而不是变体级（CF-6 冻结，不改 schema），所以由部署画像 `profile.go` 的 `portVariants` 限定，与 §10.4 表一致。
- **部署画像在 resolver 内**（运行用户、单元、文件、权限）：它是「Tools 对组件落地形态的知识」，来自 §8/§11.3/§15，不是清单内容；未知组件取最小画像（只有 release 目录）。`steps` 只是高层步骤，与 §12.1 的 22 步映射留给 1.33。
- **`InstalledVariants` 可选输入**：state.json schema v1 不记变体（严格解析，加字段须升 schema，不在本 Story 范围）。缺失时多变体且无 default 的被动升级候选被拒并提示显式选择，而不是猜一个。
- **规模上限**：回溯 20 万步，超出报 `resolve.search_limit`（防病态清单；确定性，按步数而非时间）。

## 变异验证（临时改动 → 测试必须失败 → 还原）

脚本逐条最小改动并保持可编译（`if false`/`&& false`/条件反转），运行对应包测试，立刻还原；每条结束后与改动前**逐字节一致**（脚本内 `filecmp` + 内容比对）。45 条，**44 条检出**（括号为首个失败的测试）：BUILTIN 判定、unreleased 判定（`TestBuiltinAndNotReleased`）；候选不过滤吊销、固定版本不查 CheckTarget（`TestRevokedVersions`）；候选/固定版本不查 min_upgrade_from（`TestMinUpgradeFrom`）；被动升级无需确认/不标记（`TestPassiveUpgradeFlagAndConfirm`）；requires/conflicts/反向约束不检查、内部依赖不进闭包、不回溯、环不检测、无解不给冲突链（`TestPropertyRandomGraphs`——与独立暴力枚举对照）；候选不按最高优先、staging 不计磁盘、topo 不排序（`TestMergedPlanServerAdminWebWebsite`）；预发布入选（`TestPrereleaseOnlyWhenPinned`）；允许降级候选（`TestRequiresComponentsRemoteAssumed`）；外部依赖版本不检查/缺失一律放行/server 的远端放行失效、OS 不检查（含「只有一条阻断」的断言）；内存不比较/内存未知放行/磁盘不比较/只读不查（`TestResourcesMemoryAndDiskPerMount`）；挂载点不取最长前缀/不判路径边界（`TestMountFor`）；4 条地址冲突语义（`TestOverlapMatrix`）；tcp6 不归一、自有监听不豁免、与本机监听冲突不检查、ssr 端口不按变体过滤（`TestPortsAgainstListeners`/`TestOwnListenerIsNotAConflict`/`TestWebsiteSSR…`）；组件间端口冲突/keep 之间误报（`TestPortsBetweenComponents`）；`--set` 不生效（`TestPortOverrides`）；cli 的端口/内置退出码映射与「未接线 Run.finish」（`TestResolveErrorMappingIsExhaustive`/`TestResolverFailuresThroughFinish`）。
- **未检出 1 条（等价变异，如实披露）**：`sortedKeys` 去掉排序。依赖边的构造顺序对输出没有影响（拒绝原因在返回前已排序、引入者取当前组件、topo 的依赖表单独排序、警告/阻断出口统一排序），所以该排序是纵深防御而非行为所必需；保留它以免未来改动引入 map 迭代序依赖。
- 变异过程中**自己的脚本缺陷**：首版有 4 条因变量被闲置而编译失败（脚本把「编译失败」单列为无效而不计入检出），已改写为可编译变异；另外 2 条原先「未检出」暴露了**测试**的弱点（OS/架构不支持的用例只断言退出码，OS 检查被去掉后其他阻断也会给出 3；runtime 缺失的放行变异没有命中有效分支），已加强断言与变异后重跑。

## 对既有测试/防护的改动（逐条）

- **没有修改任何既有测试、门禁规则或白名单。** `cli/run.go` 增 `ResolveError` 分支并把 resolver 错误排除在 `isParseStageError` 之外（与 manifest/downloader/registry 同处理）；退出码表（0/1–14/130）、envelope schema、`--help` 输出未变，`exitcode_test`、help/envelope 快照仍通过。
- 新增文件：`internal/tools/resolver/*`、`internal/tools/cli/resolveerr.go`、`resolveerr_test.go`；文档 `tools-design.md`（§10.5 与修订记录）、`sprint-status.yaml`。

## 测试（真实、无 Mock）

- 测试清单经**真实的 `manifest.ParseManifest`** 生成（不手工拼结构体绕过生产解析）；已装状态用真实的 `registry.State`；cli 测试经真实的 `Run.finish` 与 envelope schema 校验。没有任何 mock/桩。
- **确定性**：同一输入 50 次、重建清单（Go map 迭代序每次不同）并打乱 Want/Listeners/Disks 顺序 200 次，JSON 逐字节相同；纯度护栏 `TestPackageIsPure` 扫描非测试源码，禁止 import `os/io/time/math/rand/crypto/rand/net/syscall/os/exec` 等；`TestResolveDoesNotMutateInput` 证明不修改输入。
- **属性测试**（`prop_test.go`）：4000 个随机依赖图（2–5 个组件、1–3 个版本、requires/peer/conflicts/内部组件，约 3565 可解 / 435 无解）——soundness（结果满足全部约束、在场集合恰为闭包、order 是拓扑序）、completeness（与独立暴力枚举器对照：存在解当且仅当 Resolve 给出解）、determinism（重复与打乱）；`FuzzResolveProperties` 以 int64 种子驱动同一检查，本机 25 秒 63k 次执行无失败（`testdata/fuzz` 无新增文件）。

## 门禁结果

见提交说明与交付报告（`gofmt -l .` 无输出；`make lint security gate-selftest race vuln contract-check build verify-build migrate-test` 与 linux/amd64、arm64 交叉编译）。

## 遗留与风险（只列确属后续 Story 或需特定环境的）

- **代码已完成、待环境验证**：无。本 Story 是纯函数，本机已全部测完；`System` 的真实产出（`/proc/meminfo`、`statfs`、`ss`、宝塔探测）属 1.31（ENV-LNX/ENV-BT）。resolver 对 Linux 真实输出的假设（`MemAvailable` 语义、cgroup 推得的 `unit`、`ss` 的地址形态）在 1.31 的真实探测样本上复验，不是本 Story 的验证项。
- **属后续 Story**：1.31 产出 `resolver.System`（含 `Listener.Unit` 的 cgroup 推导与 `Baota`）；1.33 接线 `install/update`：调用 `Resolve`、展示 Plan、`--dry-run` 返回 `plan.Err()`、确认被动升级后以 `AcceptPassiveUpgrade` 重新求解、把 `steps` 映射为 §12.1 的 22 步，并在知道已装变体时填 `InstalledVariants`；`--set` 的命令行语法到 `PortOverrides` 的解析（§3 标志属 1.33）；Plan 渲染（人类输出/TUI 计划确认页）属 1.33/1.38。
- **已知限制**：`0.0.0.0` 对 `::` 的冲突按双栈处理（`ss` 无 `IPV6_V6ONLY` 信息）；磁盘需求只记 release 目录与 staging（manifest 无数据目录需求字段）；`Plan.Files` 是声明式清单，不检查磁盘上是否已存在（纯函数；存在性由 1.33 的幂等步骤 `registry.EnsureFile` 判定「已存在且一致/冲突」）。
