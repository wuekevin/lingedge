# ADR-012 Tools 发行包签名（Ed25519 + SHA256 清单）与三个信任域

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §4.5、§4.7、§4.8、§24；PRD FR-8、FR-9、FR-26、NFR-15、D-12、OQ-8；architecture AD-12；细节见 tools-design §9｜阶段：M1

## 背景

基线 §4.8：数字签名验证、禁止未验证发行包、支持离线安装可信发行包；§4.5：下载签名版本清单→验证清单签名→下载包→校验 SHA256 与签名。PRD D-12：三个**独立信任域**（发行清单签名、配置版本签名、节点 mTLS CA），各自可轮换、可吊销；OQ-8 要求架构给出算法与保管方式。

## 决策

### 1. 发行信任域

1. **算法**：**Ed25519**（签名）+ **SHA-256**（摘要）。理由：Go 标准库原生支持、密钥/签名短、确定性签名、无 RNG 失败风险。
2. **清单**：`manifest.json`（版本清单）+ `manifest.json.sig`（JSON：`{key_id, alg, sig}`；签名对象 = `"LINGEDGE-MANIFEST-V1\n"` ‖ **`manifest.json` 文件原始字节**，Ed25519。**不做 canonical JSON**——对文件字节签名免去规范化歧义；以 tools-design §9.2/§9.6 为唯一规范并附示例向量）。清单内每个组件版本、每个平台包含 `url`、`size`、`sha256`；另含 `sequence`（单调递增整数）、`generated_at`、`valid_until`、`revoked_versions`（被吊销的组件版本）、`requires`/`migrations`/`min_upgrade_from` 元数据（schema 见 tools-design §9.2）。**清单内不含 `revoked_key_ids`**（见第 4 点）。
3. **包**：`lingedge-<component>-<version>-<os>-<arch>.tar.gz`（标准库 `compress/gzip`，已定，不再保留 `.tar.zst` 备选）；包内 `META/SHA256SUMS` + `META/SHA256SUMS.sig`（同一发行密钥；签名对象 = `"LINGEDGE-PKG-V1\n<component>\n<version>\n<platform>\n"` ‖ `SHA256SUMS` 字节，tools-design §9.3）覆盖包内每个文件，所以"清单摘要"与"包内签名"是**双重校验**：外层 sha256（防传输损坏/替换）与内层签名（防清单被替换后包体不匹配的组合攻击）。
4. **公钥集**：`keys.json` 内置于 `lingedge`（`go:embed`），含多公钥 `{key_id, public_key, status, not_before}`；校验时接受清单声明 `key_id` 且状态为 `active|retiring` 的公钥；**密钥吊销列表** `revoked_key_ids` 位于 `keys.json`（内置 + `trust/keys.d/` 合并，随新版 Tools 发行分发），任一为吊销即拒绝；**版本吊销** `revoked_versions` 位于清单。**公钥轮换**：新版 Tools（用旧 key 签名发布）内置新旧两组公钥，重叠期后移除旧 key。
5. **私钥保管**：发行签名私钥**离线保管**（隔离环境/硬件密钥，不在 Server、不在 CI 明文），发布流水线产出待签清单 → 人工/离线签名 → 回传 `.sig`。CI 中不存放私钥（供应链风险）。
6. **防回滚/冻结**：Tools 本地记录已见最大 `sequence`（`state.json`）；清单 `sequence` 小于本地记录**一律硬失败**（退出码 5，**没有** `--allow-rollback` 之类的绕过开关；确需回到旧版本须由发行方发布更高 `sequence` 的清单）；`valid_until` 过期拒绝（`--offline` 且附"离线包"时给明确警告与 `--allow-expired-manifest`，不静默接受）。
7. **校验顺序**（每次 install/update，任一步失败退出码 5）：下载清单与 `.sig` → 校验 `key_id` 可用且未吊销 → 验证签名 → 检查 `sequence/valid_until` → 选版本 → 下载包 → 校验 `size` 与外层 `sha256` → 安全解压到临时目录 → 校验 `SHA256SUMS.sig` → 逐文件校验 → 才允许进入安装（防路径穿越见 tools-design §18）。
8. **离线包**：`lingedge-offline-<release>.tar`，内含 `manifest.json`、`manifest.json.sig`、各包与 `keys.json` 增量；用**同一条**校验链；`--offline <bundle>` 不联网。
9. **透明度**（可选，后续）：把清单哈希发布到独立渠道（官网/Git 标签）供交叉核对；M1 不强制。
10. **SBOM**（NFR-15 [ASSUMPTION]）：发布产出 SBOM（Go：`go version -m` + 依赖清单；npm：lockfile；工具实现时核对），随清单发布；构建可复现性为目标（固定 toolchain、`-trimpath`、`-buildvcs=false` 等，实现时核对）。

### 2. 三个信任域（互不共享密钥）

| 域 | 保护对象 | 算法 | 私钥位置 | 公钥/信任锚分发 | 轮换 |
|---|---|---|---|---|---|
| 发行清单 | 组件包与版本清单 | Ed25519 + SHA-256 | 离线 | 内置于 Tools（多公钥 + 吊销列表） | 新旧并存的重叠期（随 Tools 升级分发） |
| 配置修订 | Config Revision / keyset | Ed25519 | Server `signing_keys`（信封加密，ADR-011） | enroll 时下发初始 keyset；链式 keyset 轮换（ADR-002、node-protocol §8） | 重叠期内新旧均可验证；无过期（R-4） |
| 节点 mTLS CA | 节点↔Server 身份（亦签发 Server gRPC 叶证书） | ECDSA P-256，CA 默认 10 年、叶证书 90 天 | Server `signing_keys`（信封加密，ADR-011） | CA bundle 随 enroll 下发；Enroll Token 内钉扎 CA 根指纹 | 分阶段：新 CA 入 bundle 并全员确认（`ca_bundle_version`）→ Server 与新节点证书改由新 CA 签发 → 节点续期 → 撤旧 CA（ADR-003 的 CA 轮换） |

附：会话/API Token/enroll Token 不属于签名体系（高熵随机 + 哈希存储，ADR-018、ADR-003）。

## Binds / Prevents / Rule

- 约束：发布流水线、`internal/tools/manifest|downloader|security`、Node 升级路径（ADR-024）、离线包。
- 防止：未验证包被安装；清单/包被替换或回滚到旧版本；一个域的密钥泄露波及其他域；CI 被攻破即可签发恶意发行包。
- 规则（AD-12）：**发行清单与包 Ed25519 + SHA256 双重校验；信任根内置于 Tools、私钥离线；三个信任域密钥独立。**

## 备选方案与权衡

- **minisign/cosign/sigstore**：功能成熟但引入外部工具/透明日志依赖，离线与自部署场景复杂；Ed25519 自定格式简单且可纯 Go 实现。保留未来兼容（可推翻）。
- **GPG**：可用性与密钥管理负担重，Go 实现庞大。
- **TLS 下载 + 仅 SHA256（无签名）**：否决——被入侵的下载源即可同时替换包与哈希。
- **单一密钥签三类对象**：否决（D-12）——泄露波及面过大。
- **RSA/ECDSA**：Ed25519 更简单、无随机数依赖。

## 后果

- 发行流程需要离线签名环节，发布节奏受限于人工步骤；可用硬件密钥与脚本降低成本。
- 自定义清单格式需要自行维护规范与测试（tools-design §9）。

## 可被推翻处

算法（Ed25519）、包压缩格式、是否采用 minisign/sigstore、`valid_until` 策略；多签/门限签名（如 2-of-3 发布者）可作为加强项。

## 验证

- 篡改清单、`.sig`、包体、包内文件任一，Tools 拒绝（AC-J-2）；`sequence` 回滚与 `valid_until` 过期拒绝；吊销 key 拒绝；离线包同样校验（AC-J-7）。
- 密钥轮换演练（新旧并存）；CI 中不存在发行私钥（仓库与流水线密钥扫描）。

## 修订记录 2026-11-16

- **IMP-18**：签名输入以 tools-design §9 为准回写——清单签名对象为 `manifest.json` 文件原始字节（删除 `canonical_json`），吊销拆分为 `keys.json.revoked_key_ids` 与清单 `revoked_versions`（清单内不含 `revoked_key_ids`），包格式确定为 `.tar.gz`，包内签名对象明确；tools-design §9.6 新增示例向量供两个独立单元（离线签名流水线与 Tools 验签）交叉验证（AC-J-2）。原因：两文档按各自描述实现会互不兼容。
- **M16**：删除 `--allow-rollback`（Tools 对 sequence 回退是硬失败、退出码 5），`--allow-expired` 改为 `--allow-expired-manifest`，与 tools-design §3/§9.2 一致。
- **M13 / IMP-05**：§2 三信任域表中“节点 mTLS CA”一行补充 CA/叶证书有效期与分阶段轮换流程，并注明该 CA 同时签发 Server gRPC 叶证书（decisions §8）。

## 修订记录 2026-11-21（Story 1.23）

- **§2 落地为可执行门禁**：「发行签名私钥离线、Server 不持有」不再只是文字约束——`make security` 的 `gates trustdomain` 扫描 Server 代码路径（`internal/`（不含 `internal/tools`）、`cmd/lingedge-server`、`pkg/`）与 Server 示例配置，出现「加载发行清单私钥」的标识符/字符串字面量（含发行清单签名域前缀 `LINGEDGE-MANIFEST-V1`，它只属于 Tools 验签与离线签名流水线）即失败；`tests/security/trustdomain` 对真实编译的 `lingedge-server` 做 `go tool nm` 符号扫描；两处都带负向样本（加入 `loadReleasePrivateKey` 必被抓到）。`signing_keys.purpose` 的 CHECK 只含 `config_signing|mtls_ca|grpc_server`，schema 上也没有发行清单用途。
- 信任域②③在 Server 的实现见 `internal/platform/signingkeys` 与 `pkg/signing`（节点侧复用的验证库，只依赖标准库）。
