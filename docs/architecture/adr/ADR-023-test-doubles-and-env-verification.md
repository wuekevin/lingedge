# ADR-023 测试替身隔离与「等待环境验证」策略

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §1.3、§23、§25；PRD NFR-1、NFR-3、NFR-28、R-11、SM-C1；architecture AD-23；[dev-environment §6–§7](../dev-environment.md)｜阶段：M1 起全程

## 背景

基线 §1.3 禁止用 Mock API、模拟节点、虚假数据冒充生产，但 §23 又要求大量测试，允许 Mock/Fixture/模拟服务器，**前提是与生产实现严格区分**（R-11）。基线 §25：测试环境缺失时必须标记「等待环境验证」，不得写成已完成。本机（macOS arm64）有 Go、Node、MySQL 8.0.46、Redis 8.10.1、Homebrew nginx，**没有 OpenResty，也没有 systemd/宝塔/Linux 多机**。Node 的核心价值（Lua 引擎、缓存、真实 IP、`nginx -t`）依赖 OpenResty。

## 决策

### A. 测试替身隔离（NFR-1）

1. 替身只能位于：`_test.go` 文件、`tests/` 目录、`internal/**/testdouble/` 包；后者所有文件带 `//go:build lingedge_testdouble`。
2. 生产构建（`make build`/`make release`）**不带**该标签；CI 反向检查：对每个生产二进制执行 `go list -deps`（不带标签）与 `go version -m`/符号扫描，**任何包含 `testdouble` 的包路径出现即失败**；并检索产物中固定的假数据标记（样例节点名、示例流量数字等）。
3. Fixture（SQL、JSON、证书/密钥）只在 `tests/fixtures/`，生产代码不得 `embed`；测试证书/密钥标注「仅测试」；测试口令运行时随机生成（NFR-6）。
4. 前端 mock（如 MSW）只在 `*.test.ts`/`tests/`；Admin/Console/Website 生产构建检测环境变量与 bundle 中不含 mock 入口（ADR-014）。
5. **验收类证据只接受真实组件**（基线 §27 闭环与 PRD AC 表）：用替身取得的「通过」不得计入 SM-1（SM-C1）；测试记录中每条证据标注 `real|testdouble`。
6. 生产代码中不得有「当检测到测试环境时走另一条路径」的分支（禁止 `if testing {}` 式后门）；差异只能经依赖注入在测试装配层产生。**真实 IP 调试（FR-73）同样遵守**：它不是请求路径上的调试分支，而是 ctl socket 的封闭操作 `debug_simulate`，以**与请求路径同一批纯函数**（`le.realip.resolve`、`origin_headers.build`）计算，对端地址只是函数入参（ADR-006 §10）；因此生产引擎中不存在“可被请求影响的模拟分支”。

### B. 无 OpenResty 的本机 Node 集成测试：分层与标记

7. 测试按 L1–L7 分层（完整表见 dev-environment §6）。要点：
   - **L1/L2（Go 单元、渲染器 golden、注入向量的静态断言）**：本机可运行；其「通过」仅表示 Go 侧逻辑正确，**不得据此声称配置可被 OpenResty 加载**。
   - **L3（真实 `openresty -t`）、L4（Lua 引擎）、L5（Agent+OpenResty 集成：签名修订应用、灰度、回滚、真实 IP、缓存 HIT/Range/锁、WebSocket/SSE、主控离线继续服务、失效 PoC）、L6（多机端到端，闭环 A/C/D/E/J）、L7（性能基线）**：需要 Linux + OpenResty（L6/L7 需多机/参考硬件），**本机无法运行**。
8. macOS 上的 Homebrew nginx（无 Lua）**不得**用于 L3–L7，也不得把它的 `-t` 结果当作通过；用 Mock 冒充 OpenResty 同样禁止。
9. Server 侧可在本机真实运行（MySQL/Redis 为真实服务；**DDL 规则与 M1 迁移顺序骨架已在本机 MySQL 8.0.46 的临时实例上真实执行**，docs/database/verification/，不使用任何替身）；最低版本 8.0.16 的空库迁移需 CI/验收机执行，标「等待环境验证」；Node 侧 Agent 的 Go 逻辑可在本机以「不启动 OpenResty」模式做 L1/L2；涉及 OpenResty/systemd 的一律在 Linux 验收机。
10. 提供 `make edge-test` 的分级行为：探测到 `openresty` 与 `resty` 才执行 L3/L4，否则**明确输出 `SKIPPED: openresty not found`** 并以非成功的专用退出码或标记写入测试报告（不得静默当作通过）。

### C. 「等待环境验证」状态

11. Story 状态机含 **`等待环境验证`**（介于「开发完成」与「完成」之间）。凡 DoD（基线 §25）中「集成测试通过」「运行验证完成」依赖 L3–L7 而环境缺失时，Story 必须进入该状态，**不得标记完成**。
12. 记录要素（写入测试记录模板）：缺失的环境（如「Linux + OpenResty」「ARM64 节点」「宝塔机」）、已完成的层（如 L1/L2 通过）、待执行的验证步骤、验证人/日期（留空待填）。
13. 阶段/闭环验收：只要有关键 AC 处于「等待环境验证」，对应闭环**不得整体标记通过**；MVP 出口判据列出全部此类项（PRD §10.1）。
14. 建议第一批 Linux 验收机在 Epic 3 早期（PoC-1/2/3）就位；若迟迟不到位，**在汇报中明确风险**，而不是降低 DoD 或放宽状态定义。

### D. 测试记录

15. 每次记录：环境、步骤、实际结果、性能基线、未通过项、已知问题（基线 §23）；**禁止虚构测试结果**（NFR-28）。性能数值只来自真实压测输出，未跑则写「未执行」。

## Binds / Prevents / Rule

- 约束：全部开发者、CI、Story 状态管理、验收记录。
- 防止：测试替身泄漏到生产产物；以本机 nginx/Mock 的通过冒充数据面验证；把缺环境的 Story 误报完成。
- 规则（AD-23）：**测试替身只存在于 `_test.go`/`//go:build lingedge_testdouble`，生产构建 CI 检查不可达；依赖缺失环境的验证一律标「等待环境验证」。**

## 备选方案与权衡

- **在 macOS 上编译/安装 OpenResty 作为必经路径**：否决——macOS 非目标平台，行为差异（事件模型、缓存/文件系统）使结果不可作为 Linux 验收证据；仅可选用于开发便利，且结果不计入验收。
- **用 Docker 提供 OpenResty 测试环境**：否决——基线禁止将 Docker 作为必选组件；不应让测试链路依赖它（开发者个人使用不受限，但不得成为 CI/验收依赖）。
- **用 Mock/fake nginx 进程模拟数据面**：否决——正是基线禁止的「模拟节点代替真实节点」。
- **缺环境时放宽 DoD 直接标完成**：否决——违反基线 §25。

## 后果

- Node 相关 Story 的完成依赖 Linux 验收机，存在排期风险；需要尽早准备环境（OQ-1、D-14）。
- CI 需要 Linux 任务（带 OpenResty）才能关闭 L3–L5；纯 macOS 开发者只能推进到「等待环境验证」。
- 额外的构建约束与反向检查增加少量工具链工作。

## 可被推翻处

若用户提供持续可用的 Linux CI/验收环境：L3–L5 可自动化并从「等待环境验证」中消除；若用户同意把容器化测试环境作为**非必选的开发辅助**，可记录在 dev-environment，但不得改变验收证据标准。

## 验证

- CI：生产二进制不含 `testdouble` 包（反向检查）；`make edge-test` 在缺 OpenResty 时输出 SKIPPED 并在报告中记为未验证。
- 抽查已完成 Story 均有 DoD 证据；「等待环境验证」项在测试报告中单独列出（SM-5）。
- 验收记录中 `real|testdouble` 标注与 SM-C1 的零计数核对。

## 修订记录 2026-11-16

- §A.6（IMP-11、M11）：补充真实 IP 调试不违反“生产代码无测试分支”（纯函数 + ctl 封闭操作，无请求层模拟分支）。
- §B.9（BLK-01）：记录 DDL 实测在本机真实 MySQL 上执行，8.0.16 最低版本验证标「等待环境验证」。
