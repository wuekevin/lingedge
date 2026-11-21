# ADR-027 systemd 版本门槛与主密钥交付的降级方案（LoadCredential 不可用时）

- 状态：已接受（Story 1.32 实现时决策，**默认采纳、可推翻**）｜日期：2026-11-21｜关联：ADR-011 §2/§9、ADR-024、tools-design §15.1/§21；PRD FR-6、FR-12、NFR-12/13；Story 1.32｜阶段：M1

## 背景

ADR-011 §2 规定 Server 的主密钥经 systemd `LoadCredential=master.key:/etc/lingedge/server/master.key` 交付：原文件 `root:root 0400`，运行用户 `lingedge` 无法直接读取，进程只读 `$CREDENTIALS_DIRECTORY/master.key`。tools-design §21 把「`LoadCredential` 的最低 systemd 版本」留给实现时核对，并要求「不满足时回退为『root 读取后降权』或 `EnvironmentFile`（权限 0400），降级方案须 ADR 记录」。

核对结果（systemd NEWS；真实发行版上的复验登记为 ENV-LNX，见「验证」）：

| 项 | 版本 | 依据 |
|---|---|---|
| `LoadCredential=` 与 `$CREDENTIALS_DIRECTORY` | **247** | service credentials 随 v247 引入 |
| 加固单元整体可用的最低版本 | **235** | 模板用到的指令中最晚引入的是 `LockPersonality=`、`RuntimeDirectoryPreserve=`（均 235）；`MemoryDenyWriteExecute=`/`RestrictRealtime=` 231、`ProtectKernel*=`/`ProtectControlGroups=` 232、`RestrictNamespaces=` 233 |

常见发行版：Debian 10（241）、RHEL 8（239）、Ubuntu 20.04（245）**低于 247**；Debian 11（247）、Ubuntu 22.04（249）、Debian 12/RHEL 9（252）满足。RHEL 7（219）低于 235，不在支持范围。D-14 的验收环境包含 RHEL 系，故降级路径是真实会被走到的。

## 决策

1. **版本判定**：`service.ParseVersion` 解析 `systemctl --version` 第一行（`systemd 252 (252.22-1~deb12u1)`、`systemd 245.4-4ubuntu3.22` 等），`SelectCredentialMode`：`< 235` 拒绝（`unsupported`）；`235 ≤ v < 247` → **降级变体**；`≥ 247` → 默认变体。数字是常量 `MinSystemd`/`CredentialMinSystemd`，测试断言它们等于「模板指令引入版本表」的最大值（新增指令必须登记版本）。
2. **降级变体 = 密钥文件方式**：单元**不含** `LoadCredential=`（渲染成与默认变体逐键相同的其余加固项，包括 `LimitCORE=0`、`ProtectSystem=strict`、`CapabilityBoundingSet=` 为空）；`master.key` 的规格由 `root:root 0400` 改为 **`root:<运行组> 0440`**（属主仍是 root、不可写，运行组只读）；`server.yaml` 的 `secrets.master_key_file` 填该文件的绝对路径（默认变体必须留空，否则 `pkg/secretbox` 的「配置路径优先于凭据目录」会绕过 LoadCredential；`service.MasterKeyConfigValue` 给出两种模式的取值）。
3. **为何不是 tools-design §21 列的另两个方案**：
   - 「**`EnvironmentFile` 0400**」：systemd 以 root 读取、再把变量注入服务进程，文件本身确可保持 root 0400。但 Server 的 `secretbox.FileKeyProvider` 只认文件，需要新增「从环境变量读」的路径；且环境变量会出现在 `/proc/<pid>/environ`（同用户可读）、被子进程继承，比文件更易泄露，与 ADR-011 §9「密钥不入日志、不放 string」的方向相反。
   - 「**root 读取后降权**」：要求 Server 以 root 启动并自行 `setuid`，与「Node/Server 全部进程非 root」（AD-24）和 `NoNewPrivileges`/`CapabilityBoundingSet=` 为空相冲突，且是更大的 Server 侧改动。
   - 现状下 Server 已支持「配置的 `master_key_file`，权限 0400/0440，属主为 root 或运行用户」（`pkg/secretbox` 的 `checkMode`/`defaultAllowedUIDs`），**降级变体无需任何 Server 代码改动**，且 `LoadCredential` 与密钥文件在进程内都是「一个只含 KEK 的文件」，威胁面差别有限（见后果）。
4. **doctor 告警**（`service.CheckSystemdVersion`、`CheckCredentialConsistency`，供 1.42 使用）：`< 235` → fail；`235..246` → **warn**「主密钥已降级为密钥文件方式」；单元含 `LoadCredential=` 但本机不支持 → fail（服务无法启动）；单元是降级变体而本机已支持 → warn（可重装单元并把 `master.key` 改回 `root:root 0400`）。
5. **安装路径**：安装步骤先 `systemctl --version`，`SelectCredentialMode` 选模式，据此渲染单元、按 `SpecMasterKey(mode, runGroup)` 创建 `master.key`（`O_EXCL`，**从不覆盖**）并决定 `secrets.master_key_file`（编排属 1.33）。

## Binds / Prevents / Rule

- 约束：Server 单元模板与安装器的 `master.key` 创建、`server.yaml` 生成、doctor。
- 防止：在不支持 `LoadCredential` 的系统上装出「启动即失败」的单元；降级时悄悄放宽其余加固项；降级却不告警。
- 规则：**能用 systemd credentials 就用；不能用时降级到 root 属主、运行组只读 0440 的密钥文件，并且 doctor 持续告警。主密钥文件任何时候不入备份、不由 Tools 覆盖。**

## 备选方案与权衡

见决策 3。另：不降级、直接拒绝 `< 247`——否决，会把 Debian 10/RHEL 8/Ubuntu 20.04 整体排除在外，而它们仍在 D-14 的验收范围内。

## 后果

- 降级变体下，运行组（`lingedge`）可读 `master.key`：同一台机上以 `lingedge` 身份运行的其他进程（Server 本身除外，只有它以此身份运行）理论上可读主密钥；LoadCredential 下该文件对 `lingedge` 不可读，但 Server 进程拿到的凭据副本与进程内存同样处在被攻破 Server 的权限内，**被攻破的 Server 在两种方式下都能拿到 KEK**，差别在于「同组其他进程/备份误包含」这一层。因此降级是**可接受但更弱**，doctor 告警。
- `master.key` 的 `0440` 权限在备份、恢复与 `doctor --fix-perms` 中须按模式区分（`SpecMasterKey`）。
- 升级 systemd 后，既有单元不会被自动改写（FR-6：已存在的单元只写 `.new`）；`CheckCredentialConsistency` 会提示可迁回默认变体。

## 可被推翻处

若用户要求严格 root 0400 且接受 Server 代码改动，可改为「Server 增加 `LINGEDGE_MASTER_KEY_FD`/管道传递」或「root 启动 + 降权」；影响 Server 的 `FileKeyProvider`、单元模板与本 ADR。

## 验证

- 本机：版本解析（各发行版样本）、门槛边界、两个变体的 golden 与属性断言（降级变体与默认变体除 `LoadCredential` 外逐键相同）、`master.key` 规格与 `O_EXCL`、doctor 检查函数。
- **ENV-LNX（等待环境验证）**：在 RHEL 8/Ubuntu 20.04（<247）与 Debian 12/Ubuntu 22.04（≥247）上：①真实 `systemctl --version` 的解析；②两种变体的单元 `systemd-analyze verify`；③默认变体下 `lingedge` 用户无法直接读 `master.key`、进程能读 `$CREDENTIALS_DIRECTORY/master.key`；④降级变体下 Server 以 0440 root:lingedge 的文件启动成功；⑤上表中各指令的引入版本以真实发行版行为复核。
