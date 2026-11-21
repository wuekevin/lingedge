---
title: 'Story 1.35：`lingedge install server`（Tools 侧 Server 安装）——第一批终点'
type: 'feature'
created: '2026-11-22'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: '6fac38b'
waiting_env_verification: true
context:
  - docs/bmad/epics.md Story 1.35（AC1–AC6）
  - docs/prd/LINGEDGE_V2_REQUIREMENTS.md（FR-11、FR-6、FR-9、FR-26、NFR-6、F-7/F-8、AC-A-2/A-3）
  - docs/architecture/tools-design.md §13、§12.4、§15、architecture §7、ADR-011、ADR-026/027、CC-12
  - docs/bmad/stories/1-33-*.md、1-34-*.md（其「属 1.35」遗留）
---

## Intent

**Problem:** 1.33 的 22 步状态机、1.32 的 service 包、1.34 的发行流水线都已就绪，但 `lingedge install server` 仍无法真正装出一个 Server：生产 `ProductionSpecs()` 为空（server 以 `install.component_spec_missing` 失败），没有任何「Tools 装好之后真实的 lingedge-server 能起来、向导能用 Tools 给的 token 走完、`install.lock` 写出、`lingedge list` 能看到」的联调证据，`config setup-token --reset` 仍是桩，1.34 还留了「cmd/lingedge 链接 manifest 后守卫对真实产物生效」的账。

**Approach:** 不再造新机制，把已有的库接成 Server 这条最终链路，并用**真实 lingedge-server 进程 + 本机临时 MySQL/Redis + 真实向导 HTTP 流程**验证：① 生产 `serverSpec`（Start = systemctl 启动；Health = 对 REST 监听的真实 `GET /healthz`；InitConfig 不返回文件，三件套由 installer 内建）；② server.yaml 的四个监听随计划端口（换端口后 server.yaml/健康检查/向导地址一致）；③ 访问信息补齐（SSH 隧道、非回环风险提示、醒目的主密钥分开保管提示——也适用于沿用既有 master.key）；④ `config setup-token --reset`（原子替换哈希、旧 token 立即失效）；⑤ `list` 的向导进展（只读 `install.lock`）；⑥ 联调测试 `tests/integration/toolsinstall`。联调过程中**发现并修复了三个真实缺陷**（见「联调发现」）。**没有新增迁移、没有改 OpenAPI/proto、没有改 CF-6 冻结的 schema 与 contract-v1、没有新增依赖。**

## 逐条验收

- **AC1 22 步 + 向导地址**：`installer.ProductionSpecs()` 登记 `server`；真实流程（`TestInstallServerThenRealWizard`）经 `cli.Execute` 走完 22 步：用户/目录/`server.yaml`（无口令）/`master.key`（0400，已存在绝不覆盖）/`setup-token.hash`（0600，与终端交付的 token 对应）/单元（`LoadCredential`）/启动/**真实 `GET /healthz` 通过**（本机构建的 lingedge-server，`/healthz/ready` 在 setup mode 如预期 503）/`state.json`；访问信息含向导地址、`ssh -L` 隧道提示（默认回环）、主密钥短指纹 + **醒目的「必须与数据库备份分开保管」**（`kind=master_key_warning`，沿用既有 master.key 时同样给出，指纹取既有文件的）、token（`one_time`，仅一次）。`--setup-listen 0.0.0.0:8080`：预检警告 + 访问信息风险提示（`TestServerSetupListenExposedWarns`）；与换端口矛盾 → 2。
- **AC2 MySQL/Redis 仅探测并警告**：未探测到 → `external.remote_assumed` 警告（步骤 7，不阻止）；`serverSpec.ValidateSettings` 拒绝任何数据库/Redis/口令类键；Tools 不接触口令（`TestServerSpecBasics`、`TestServerInstallHappyPathWithRealHealthEndpoint`）。
- **AC3 冲突不覆盖**：既有 `master.key`/`server.yaml`/同名单元（生产规格 + 真实流程）：非交互无 `--yes` → 9 且不写任何 `.new`；`--yes` 后三者逐字节原样、`server.yaml.new`/单元 `.new` 写出、`master.key` 沿用（`TestServerExistingFilesAreNeverOverwritten`；1.33 的 `TestConflictsAreNeverOverwritten` 仍通过）。
- **AC4 退出码与降级**：非 root → 3（`TestServerInstallRequiresRoot`，且真实二进制 `TestBinaryExitCodesAndEnvelope`）；`8080/9180/9443/9444` 任一被占用 → 4（`TestServerInstallPortOccupiedExit4`：4 个端口 × 回环/通配 × dry-run/真实；`TestInstallServerRealPortOccupationExit4`：真实 `net.Listen` 占用）；换端口避开后通过；systemd < 247 → 降级并告警（单元无 `LoadCredential`、master.key 0440、server.yaml 填路径），< 235 失败（`TestServerLoadCredentialDegradation`）。
- **AC5 健康失败回滚**：生产 Health 对无监听者失败 → `KindRolledBack`（退出码 10），整树快照与安装前相同（用户/单元/目录/配置/release/伪 systemd 状态均不残留，`TestServerHealthFailureRollsBackWithoutResidue`）；**真实进程版本**：lingedge-server 真的因配置不可读而非零退出 → Health 超时 → 退出码 10 → 无残留（`TestInstallServerHealthFailureRollsBack`）。
- **AC6 向导联调（ADR-026）**：真实流程（本机构建的 lingedge-server、临时 mysqld 8.0 + 临时 redis、真实向导 HTTP 七步）：Tools 生成的 token 被真实 Server 接受；`config setup-token --reset` 后旧 token 401、新 token 可用；向导 complete → `install.lock` 写出、`runtime-credentials.json`（0600，信封加密，无明文口令）、同一进程切换为正常模式（`/healthz/ready` 200、向导路由 410）、setup token 哈希被删除/清空；`lingedge list`（普通用户）显示 server `running` + `wizard_pending` → `initialized`（JSON 与人类可读）；向导完成后 `setup-token --reset` → 3；优雅停机退出码 0。① 安装步骤本身**不**写 `migrate.yaml`、不含任何口令（整树扫描，安装后与向导后各一次）；token/数据库口令/管理员口令不出现在 Tools 输出（token 仅交付一次）、server 日志、向导响应（迁移口令按设计一次性展示除外）、配置/状态文件里。②③（真实 systemd 下 `lingedge` 用户可写 secrets 目录、`LoadCredential` 解得密文、`sudo lingedge-server migrate credential init` 后 `migrate status` 连通）**属 ENV-LNX，等待环境验证**（本机已用「按单元文件语义准备的 `CREDENTIALS_DIRECTORY`」验证了 master.key 经凭据目录读取这条路径，非真实 systemd）。

## 前序 Story「属 1.35」遗留项逐项处置

| 来源 | 项 | 处置 |
|---|---|---|
| 1.33 | server 的 `ComponentSpec` 与 `ProductionSpecs` 登记（Init/Start/Health 真实实现，`/healthz` 探测） | **已做**：`installer/serverspec.go`。生产二进制符号扫描仍证明不含假组件；`tests/security/toolsbinary` 改为断言 `ProductionSpecs()` 恰为 `server` |
| 1.33 | server.yaml（不含口令）/master.key/setup token 的生成与显示 | **已做**（1.33 已实现生成；1.35 补：监听随计划端口、主密钥醒目提示、沿用既有 key 的指纹、SSH 隧道） |
| 1.33 | `config setup-token --reset` | **已做**：原子替换哈希、旧 token 失效、持 server 锁、需确认；向导已完成/未安装 → 3 |
| 1.33 | `install.lock` 与向导联调、`lingedge list` 显示 server 状态 | **已做**（见 AC6；`list` data 追加 `setup`） |
| 1.33 | MySQL/Redis 警告细节（AC2） | **已做**（`external.remote_assumed`；不开放口令类设置键） |
| 1.34 | `cmd/lingedge` 链接 `internal/tools/manifest` 并 go:embed 内置 keys.json，使 `gates release-keys` 对真实发行二进制生效 | **已成立并补证明**：`cmd/lingedge → cli → manifest` 的链接与 `go:embed` 自 1.33 接线 install 起就在（1.34 文档与 `tests/release` 的注释已过时）。本 Story 补：`tests/release.TestProductionBuildFlags` 对**真实发行产物**断言——tools 二进制逐字节内置流水线注入的 keys.json 且无测试公钥，server 二进制不内置发行公钥（信任域分离）；`tests/security/releasekeys.TestRealBinaryLinksBuiltinKeysConsumer` 对真实 `go build` 的 lingedge 断言链接 `manifest.BuiltinKeys → cli.Env.NewVerifier → runInstall`、内嵌文件可解析并建出信任集。「含测试公钥的生产构建失败」的真实产物扫描由既有 `TestBuildWithTestKeyIsRejected`、`TestReleaseBuildRejectsBadBuiltinKeys`、`TestScanBinaryForTestKeys` 覆盖（均仍通过） |

## 联调发现（三个真实缺陷，已修复并有回归测试）

1. **单元 ExecStart 与包布局不一致（1.32 × 1.34）**：单元模板写 `/opt/lingedge/server/current/lingedge-server`，而发行流水线打包 `payload/bin/lingedge-server`，installer 解压后是 `current/bin/lingedge-server`——真实启动 ENOENT。`TestServerUnitExecStartMatchesInstalledLayout` 与联调（真实进程按已安装单元的 ExecStart 启动）会抓到它。修复：模板默认路径改为 `current/bin/lingedge-server`（service golden/测试、tools-design §15.1 同步）。**node 的同类路径（`current/lingedge-node`）属 Epic 3，见遗留。**
2. **`--json` 下 setup token 被遮蔽（1.33）**：43 字符 base64url token 恰好满足 `pkg/redact` 的裸 base64 密钥启发式，envelope 里变成 `***`，操作者在 `--json` 下永远拿不到令牌（1.33 的测试只断言「非空」所以没发现）。修复：envelope 对**唯一交付通道**（`kind=setup_token` 且 `one_time=true` 的 `value`）做窄口还原，其余位置同形态字符串仍被遮蔽（`TestOneTimeTokenRestoreIsNarrow` 含 4 个负向样本）。
3. **`Access.secret` 布尔值被脱敏成字符串 `"***"`（1.33）**：脱敏按键名，`secret` 键被整体替换，违反 `data.install` schema。修复：JSON 键改名 `one_time`（该字段此前在 JSON 里始终不可用，未发布）。

## 测试

- **真实**：`tests/integration/toolsinstall`（3 个顶层用例）——`cli.Execute` + installer + 生产 server 规格；本机 `go build` 的真实 lingedge-server 进程；`tests/mysqltest`/`tests/redistest` 临时实例；真实向导 HTTP；真实文件系统权限；真实 `net.Listen` 占用；`kill`/SIGTERM。**伪的只有「操作系统」**：伪 systemctl/useradd/chown（`tests/toolsworld`，根目录前缀到临时目录，测试非 root）。伪 systemd 不会 fork 进程，所以测试在生产 `Start`（伪 `systemctl start`）之后，按**已安装的真实单元文件里的 `ExecStart` 与 `LoadCredential`** 启动真实二进制（路径前缀到临时根；`CREDENTIALS_DIRECTORY` 由测试按 LoadCredential 语义准备；`server.yaml` 取自已安装文件，只改写数据目录、令牌哈希路径、argon2 下限，**不改监听端口**）。这个包装器只存在于 `tests/`，生产二进制不含。
- `internal/tools/installer/server_test.go`：渲染 golden（默认/降级/换端口/暴露 4 份，`testdata/`）、监听规划、`ServerHealthURL`、规格基本契约与生产 Health（真实 HTTP：200/503/无监听者）、回滚无残留、真实 `/healthz` 监听者下的完整安装、冲突不覆盖、LoadCredential 降级、风险提示、矛盾拒绝、令牌重置（原子/权限/chown 先于 rename/旧 token 失效/已完成拒绝/取消）、ExecStart 与包布局一致。
- `internal/tools/cli/server_cmd_test.go`：端口占用 4（含 dry-run）、非 root 3、dry-run 整树零变化与计划内容、`list` 向导进展（JSON schema + 人类可读）、`config setup-token` 全部退出码与 token 不进日志、envelope 窄口还原的负向样本。
- `tests/tools/serverconfig`：随计划端口渲染的 server.yaml 被 Server **真实的 `config.Parse`** 接受且监听一致；metrics 非回环在 Tools 与 Server 两侧都被拒绝。

## 变异验证

对关键防线做临时改动 → 对应测试必须失败 → 还原并逐字节比对，共 16 条**全部被检出**：Health 恒成功、监听不随换端口、`ProductionSpecs` 混入额外规格、`ValidateSettings` 放行一切、token 还原放开所有 kind、token 还原删除、reset 不查 `install.lock`、reset 哈希 0644、reset 不 chown、ExecStart 回退旧路径、不输出主密钥醒目提示、沿用 master.key 不给指纹、`list` 恒报已初始化、非回环无风险提示、metrics 不校验回环、健康失败不回滚。

## 对既有测试/防护的改动（逐条，均为加强或随功能交付的必要更新，无放宽）

| 文件 | 改动 |
|---|---|
| `tests/security/toolsbinary/binary_test.go` | `TestProductionSpecsAreEmpty` → `TestProductionSpecsAreOnlyDeliveredComponents`：断言 `ProductionSpecs()` **恰为** `server` 且类型不是测试替身（原为「必须为空」，随 server 交付而更新，仍禁止多登记任何规格）；符号/字符串扫描未改 |
| `internal/tools/cli/behavior_test.go` | `config` 现已有已交付子命令，从「未实现命令不得出现在 --help」清单移到「已交付可见」；`config setup-token` 从「未实现 → cli.not_implemented」清单移除（其行为由新测试覆盖） |
| `internal/tools/cli/testdata/golden/help-root.json` | 重新生成：`--help` 现列出 `config`（仅此一处差异，已人工核对） |
| `internal/tools/cli/install_cmd_test.go` | 「规格缺失」用例由 `install server` 改为 `install node`（server 已有生产规格；node 仍未交付） |
| `internal/tools/cli/validate_test.go` | `TestShellMetacharactersAreLiteral`：`--set k=<元字符>` 的期望由 5 改为 2（server 规格不认识键 `k`，在任何副作用前被 schema 拒绝；值依旧只当数据、哨兵文件断言不变）；新增合法 `--set env=prod` → 5 以保持「走到清单验证」的覆盖 |
| `internal/tools/installer/install_test.go` | F-7 断言由「指纹文本含分开保管」改为「指纹 + 独立的 `master_key_warning` 含分开保管」 |
| `internal/tools/service/*`（`params.go`、`render_test.go`、`inject_test.go`、两份 golden） | server 单元 `ExecStart` 路径 `current/lingedge-server` → `current/bin/lingedge-server`（联调发现 1） |
| `internal/tools/cli/schema/data.install.schema.json`、`data.list.schema.json` | 追加式：访问类型 `ssh_tunnel`/`master_key_warning`、`list.components[].setup`；`access[].secret` → `one_time`（联调发现 3）。新增 `data.config-setup-token.schema.json` |
| `tests/release/e2e_test.go` | `TestProductionBuildFlags` 去掉过时注释，改为对真实产物的内置公钥断言（见上） |
| `tests/toolsworld/world.go` | 追加 `Options`（`ServerBinary`、`ProductionServerSpec`）与 `NewWorldWith`；缺省世界行为不变 |
| `docs/architecture/tools-design.md` | §13 实现口径、§12.4 依赖与分层一句、§15.1 ExecStart、修订记录 |

**未改动**：`SafeExtract` 与任何限额、`safeexec.DefaultAllowed`、`.golangci.yml`、`scripts/gates`、既有门禁规则与词表（G-6 无固定口令、G-11 testdouble、release-keys、secrets…）、`internal/platform/**`、迁移（000001–000018）、OpenAPI/proto、CF-6 schema、contract-v1、退出码表。

## 与任务/通常做法不同的设计决定（须披露）

1. **`cmd/lingedge` 的 manifest 链接/go:embed 无需新增代码**：链接自 1.33 起已成立，1.34 文档的「待 1.35」是过时表述；本 Story 补的是对真实产物的证明，不是接线。
2. **向导监听由「计划端口」决定，而不是 `--setup-listen` 单独决定**：`--set port.server.tcp.8080=…` 换端口后 server.yaml 的四个监听、健康检查与向导地址随之一致；二者矛盾直接失败（2），不猜。
3. **联调用「单元文件语义」启动真实进程**：伪 systemd 不能 fork，故测试按已安装单元的 `ExecStart/LoadCredential` 启动；这验证了单元与包布局、server.yaml、master.key 凭据目录读取、token 哈希互通，**不等于**真实 systemd（User=lingedge、ProtectSystem=strict、真实 LoadCredential 注入）下的行为。
4. **`config setup-token` 不带 `--reset` 报用法错误（2）**，而不是「显示现有 token」（磁盘上只有哈希，无法找回）。
5. **token 重置不写 journal**：单文件原子替换，失败即原样保留旧文件，无需撤销；持 server 组件锁以避免与并发 install/update 交错。
6. **令牌哈希路径固定为默认 `/etc/lingedge/server/setup-token.hash`**：不解析 server.yaml 里可能被管理员改写的 `secrets.setup_token_hash_file`（[ASSUMPTION] 安装器生成的 server.yaml 不改它；管理员手改后重置需自行同步，登记为后续项）。
7. **`list` 的向导进展只看文件系统事实**（`install.lock`），不探测进程/HTTP；非 root 对 0750 数据目录无权进入时报 `unknown`，而不是猜测。

## [ASSUMPTION]

- 联调里 bundle 的 `platform` 标签是 `linux-amd64` 而二进制是本机（darwin/arm64）构建——下载器与 resolver 不检查二进制本身；包不是真实发行包（没有真实发行密钥，UD-3）。
- 联调中向导 complete 后 `/etc/lingedge/server/setup-token.hash` 的删除发生在测试进程的同一用户下（可删）；真实 systemd 下见「已知限制」。
- Health 窗口缺省 60s/间隔 1s 沿用 1.33 的 [ASSUMPTION]；联调用 tools.yaml 的 `install.health_timeout` 缩短（失败用例 2s）。
- `/healthz` 在 setup mode 返回 200 是 1.24 的实现事实（`tests/integration/setup` 已断言），本 Story 依赖它。

## 等待环境验证（代码已完整，待 ENV-LNX；汇总于 1.45）

| 项 | 说明 |
|---|---|
| 真实 systemd 下的完整安装（Debian/Ubuntu LTS 与 RHEL 系各一，D-14） | `useradd/chown/systemctl` 的真实行为、`User=lingedge` 下 Server 能启动、`Type=notify` 就绪通知、`ProtectSystem=strict` 下的实际可写范围、真实 `sd_booted`；LoadCredential（≥247）与降级（235–246）两条路径 |
| AC6 ②：`lingedge` 用户可写 `/var/lib/lingedge/server/secrets/`，`master.key` 经真实 `LoadCredential` 解得密文 | 本机仅验证了「凭据目录读取 master.key」这条代码路径 |
| AC6 ③：`sudo lingedge-server migrate credential init` 写出 root:root 0400 的 `migrate.yaml` 后 `migrate status` 连通 | 需要 root；属 Server 侧命令（1.24），Tools 不参与 |
| 真实 `ss`/`/proc` 端口占用 | 本机以真实 `net.Listen` 占用 + 注入的监听表验证了决策链，未验证 probe 对真实 Linux `ss` 输出的解析（1.31 的测试夹具已覆盖解析） |
| `setup-token.hash` 在 `ProtectSystem=strict` 下的清理 | `/etc` 只读 → Server 无法删除/清空哈希文件（见 tools-design 修订记录「已知限制」）；向导路由已 410，token 事实上失效，但文件残留；待真实环境决定改 `ReadWritePaths` 还是由 Tools 清理 |
| 真实发行包与发行源（UD-3） | 联调用测试密钥签名的 bundle；真实发行密钥/公开发行源待用户确认 |

## 属后续 Story（依据）

| 项 | 归属 | 依据 |
|---|---|---|
| node 单元的 `ExecStart` 路径（`current/lingedge-node`）与包布局核对 | Epic 3（node 包交付时） | node 为 `unreleased`，包布局尚未产出；联调发现 1 已修 server |
| `config show/edit/validate server`、`doctor` 对 server 的检查与 `--fix-perms` | 1.42 | epics 1.42 |
| `uninstall server`/备份 | 1.41 | epics 1.41 |
| 升级 server（含迁移决策） | 1.43 | epics 1.43 |
| 令牌重置跟随 `secrets.setup_token_hash_file` 自定义路径 | 1.42（`config` 全家） | 本 Story 的 [ASSUMPTION] 6 |
| 向导 SPA、Admin | 1.37 / 1.38 / 1.39 | epics（本 Story 备注：B2 交付） |
| Epic 1 验收与 ENV-LNX/ENV-BT 真机复验 | 1.45 | epics 1.45 |
