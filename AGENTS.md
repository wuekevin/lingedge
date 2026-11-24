# LingEdge（凌界云）开发交接说明

商业级 CDN / WAF / 智能 DNS 边缘云平台 V2.0。本文件给接手开发的 AI 代理或工程师看，是进入仓库后的第一份必读材料。

## 1. 权威资料（冲突时按此顺序）

1. `docs/prd/LINGEDGE_V2_REQUIREMENTS.md`：权威基线。
2. `docs/bmad/epics.md`：6 个 Epic、176 个 Story 的原文与验收标准（AC）。
3. `docs/bmad/sprint-status.yaml`：每个 Story 的状态与完成摘要。
4. `docs/bmad/stories/<id>-*.md`：每个已做 Story 的实现记录、变异验证、遗留项、对既有测试的改动。
5. `docs/architecture/tools-design.md`（Tools 设计）、`docs/operations/`（发布、备份恢复、诊断、升级回滚）、`docs/bmad/contract-changes/`（契约变更）。

## 2. 技术栈与约束

- 后端：Go + Gin + sqlx + MySQL 8.0 + Redis + Zap + gRPC + mTLS。前端：Vue3 + Arco（Admin/Console）、Nuxt3（官网）。边缘：OpenResty。部署：宝塔 + systemd，**不使用 Docker**。
- 前端工作区：`frontend/`（pnpm 12.18.0、Node 26.8.2、TS 5.9.3；admin、console、website、shared、setup）。设计令牌单一来源 `frontend/shared/tokens`。
- 文档与回复使用简体中文。

## 3. 进度（截至本文件提交时）

- **Epic 1**（共 45 个 Story）：1.1–1.43 已完成，另有清理型 Story 1.22b、1.27b、1.28b、1.31b、1.37b、1.39b。
- **下一个：1.44**（多选安装 UI，见 `epics.md` 中 "Story 1.44"），然后 1.45（Linux 验收机上的环境验证汇总），再进入 Epic 2–6。
- 状态以 `docs/bmad/sprint-status.yaml` 为准。

## 4. 铁律（不得违反）

- 不得用 mock 冒充生产；不得伪造测试结果；DoD 以需求文档第 25 章为准。
- **G-6（禁止固定口令）永不豁免**；测试里的口令样例必须运行时拼接。不得放宽、绕过任何门禁，不得加豁免。
- 冻结契约不得改动：`contract-v1`、迁移 `migrations-m1` 的 000001–000018（及已登记的 CC 追加）、CF-6。
- OpenAPI 只能增量，且必须经 `docs/bmad/contract-changes/` 登记；已用到 CC-13，下一个是 CC-14。
- 无法在本机真实验证的内容（Linux/systemd/root 属主、真实宝塔、真实发行源与密钥等）可标记「等待环境验证」，但**功能代码必须写全**，不留半截。报告里必须区分「代码已完成待环境验证」与「属后续 Story」。
- 日志、输出、错误信息、journal、state 文件中不得出现密钥或口令。
- 测试自行启动临时 MySQL/Redis（`tests/mysqltest`、`tests/redistest`），绝不使用机器上常驻的 mysqld；结束后确认没有残留进程（`pgrep -fl "tdata|headless|nginx"`）。

## 5. 每个 Story 的工作流程

1. 读 `epics.md` 里该 Story 的原文与 AC，以及相关设计文档。
2. 实现代码与测试（真实依赖优先；关键防线做变异验证，存活项补强测试）。
3. 创建 `docs/bmad/stories/<id>-*.md`，把 `sprint-status.yaml` 中该 Story 置为 `review`，并写明遗留项与 `[ASSUMPTION]` 假设。
4. 本地跑完整门禁（见下），**以最后一次代码改动之后的整套结果为准**，不得只跑部分。
5. 提交并推送 `main`。
6. 前序 Story 留下的、本机可做的遗留项，先做完再进下一个 Story（用 "Nb" 清理型 Story）。

## 6. 门禁（全部必须通过）

```
gofmt -l .                      # 必须无输出
make lint security gate-selftest vuln contract-check build verify-build migrate-test frontend-check
make race                       # 整套 go test -race，较慢（约 15–20 分钟），建议后台运行
make e2e                        # 涉及浏览器/前端时运行（Playwright + 真实 server + 临时 MySQL/Redis + Chromium）
```

注意事项：
- `make vuln` 含 `pnpm audit`，需要联网。
- 门禁运行期间**不要**执行 `go clean -cache`（会让并行编译报 "could not import"，造成假失败）。磁盘低于 8G 时，先停下清理，再整轮重跑。
- 不要写 `while pgrep -f "<含自身字符串>"` 这类等待循环：`pgrep -f` 会匹配到循环自己，永远不退出。
- 测试曾因并发出现过两类偶发失败（前端 `dist` 被并行重建、Playwright 临时目录被删），均已加固；再遇到偶发失败，先查根因，不要靠重跑掩盖。

## 7. 提交约定

- 提交信息中文，格式 `feat: Story X.Y 标题` / `fix: ...`。
- 提交身份：`wuekevin`，邮箱使用 GitHub 提供的 noreply 地址，不要修改全局 git 配置。
- 仓库公开（github.com/wuekevin/lingedge），分支 `main`。

## 8. 已知的待环境验证项（汇总于 Story 1.45）

- Linux/systemd/root 属主、真实 `systemctl`/`LoadCredential`（需 systemd ≥ 247）、`setup-token.hash` 在 `ProtectSystem=strict` 下的清理（决定放进 `ReadWritePaths` 还是由 Tools 清理）。
- 宝塔真实目录布局、MySQL 8.0.16 拒绝路径、宝塔下新建站点并 include 的人工路径。
- 真实发行密钥与发行源（UD-3）、OpenResty 目录树打包（UD-4 / Story 3.22）。
- 真实 Nginx + HTTPS 下的 Host/CSP/Secure Cookie、Safari/Firefox/手机端视觉（1.45）。
- 远程 CI 首次运行（`.github/workflows/ci.yml` 尚未在远程跑过）。
- UD-14 视觉令牌确认。

## 9. 后续 Story 的已知挂靠

- 1.44：多选安装 UI、TUI 的备份/卸载/升级确认/不可逆确认/状态/日志/配置页。
- 2.4 / 2.6：官网读取品牌信息；2.5：`runtime-config.js`、自用模式 `/pricing` 404、公开媒体目录；2.14：官网 SSR 变体。
- 3.x：节点升级路径（`lingedge-node-upgrade.path/.service`、`update node --apply-staged`）、节点 unit 的 ExecStart 路径、`health.node`/`node.*`/`dns.*` 检查（3.31、4.x）。
- 6.18：`secrets.rewrap` 接线与移除 KEK 命令；6.19：许可证扫描与发布密钥演练。
- SIGHUP 热加载：按 ADR-026 明确延后。
- 前端：`acting_tenant` 服务端化与租户切换器（P5）、P2 设置页、主题偏好服务端保存（需要契约变更）。
- Server 就绪探针对 expand 迁移的 N-1 兼容：目前回滚含 expand 迁移的升级时只要求存活探针与 schema 符合预期（`healthy_live_only`），待 Epic 3 统一决定。

## 10. 需要使用者拍板的事项

- UD-3：真实发行密钥/签名人/发行源。
- UD-14：视觉令牌确认。
- `tools.yaml` 缺省值（内存下限 256MB、磁盘下限 512MB、备份新鲜度 168 小时、`update.keep_releases` 缺省 2）为 `[ASSUMPTION]`，可在配置里调整。
