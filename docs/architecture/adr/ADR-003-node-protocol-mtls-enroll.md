# ADR-003 Node ↔ Server 协议、mTLS 身份与 enroll 流程

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §10.2、§19、§24；PRD FR-26、FR-27、FR-52、FR-53、NFR-14、D-12、反馈 F-2；architecture AD-3、§9.5；协议细节 `docs/api/node-protocol.md`｜阶段：M1

## 背景

基线要求 Node Agent 使用独立协议与权限体系、mTLS 身份认证、注册、心跳、拉取配置；**不得将全部节点控制权限暴露给普通租户**。D-12：节点 mTLS 私有 CA 是独立信任域，节点证书可吊销，节点身份与签名密钥分离。

## 决策

1. **传输**：gRPC（Protobuf，包 `lingedge.node.v1`）over **TLS 1.3**（最低版本即 1.3，threat-model §5.2 同步）；**节点主动发起所有连接**；节点无控制面入站端口。
2. **两个监听**：`:9444 Enroll`（仅服务端证书；仅当存在未过期 enroll Token 时监听；限速）与 `:9443 Control`（严格 mTLS）。REST 与 gRPC 不同端口、不同认证体系（FR-27）。
3. **私有 CA 与 Server 叶证书**（M13/IMP-05）：CA = ECDSA P-256，**默认有效期 10 年**（可配 5–10 年，`doctor` 剩余 <1 年告警）；CA 私钥以信封加密（ADR-011）保存于 Server（`signing_keys.purpose='mtls_ca'`），仅在签发时解密；CA 与配置签名密钥**独立**（FR-26）。**Server 自身的 :9443/:9444 TLS 叶证书由同一 CA 签发**：`ExtKeyUsage=serverAuth`，SAN = `server_url` 主机名/IP + 系统设置 `grpc.tls.san_extra`；私钥信封加密存 `signing_keys(purpose='grpc_server')`；进程经 `GetCertificate` 回调热重载；有效期 90 天、2/3 周期由 Scheduler 任务 `server.cert_rotate` 自动轮换，**轮换早于任何 CA 轮换**。enroll Token 中的 `ca_fp` = CA 根证书 DER 的 SHA-256，Agent 钉扎该指纹校验 Server 出示的链。
4. **节点证书**：叶证书由节点本地生成的私钥 CSR 签发；Server **忽略 CSR 中的 Subject/SAN**，强制 `SAN = URI:urn:lingedge:<role>:<uid>`；ExtKeyUsage=clientAuth；**有效期默认 90 天**（可配），在 1/3 剩余期（≈第 60 天）由 Agent 调用 `RenewCertificate`（新密钥+新 CSR，用当前证书认证）自动续期。**续期是两阶段的（IMP-05）**：新证书签发为 `pending`、旧证书仍 `active`；服务端拦截器在**新证书首次成功握手**时才把它置 `active`、旧证书置 `superseded`（`grace_until = min(旧 not_after, superseded_at+7d)`，宽限内仍可握手，吊销可立即切断）。响应丢失或 Agent 落盘前崩溃时旧证书继续可用，避免节点被旧证书拒绝而只能人工 re-enroll。
5. **证书过期后**：数据面继续服务（ADR-020），但无法重连主控；须 `lingedge node enroll --force-reenroll`（Admin 新建 Token，沿用 `node_uid`；CLI 标志以 tools-design §4 为准）。此为**已知限制**并上报 PRD 反馈 F-2；`doctor` 在剩余有效期 **<1/6**（续期窗口 1/3 已过半仍未续成）时告警（避免与续期阈值同为 1/3 而常态告警）。
6. **吊销**：`node_certificates` 记录序列号/状态；拦截器对每个 RPC 检查（内存缓存 + Redis pub/sub 失效，Redis 不可用时短 TTL 轮询 MySQL，ADR-025）；吊销节点（`nodes.status=disabled`）亦拒绝。不引入 CRL/OCSP 分发（节点只连自家 Server，服务端检查即可）。
7. **enroll**：Admin 创建节点记录与一次性 Token `lenr1.<base64url(server_url|ca_fp|secret)>`（`secret` ≥32B；Server 仅存 `SHA-256(secret)`；TTL 默认 24h，最小 1h；绑定**节点** `node_uid` 与 `role`（`enroll_tokens.role` 列）；可选源 IP/CIDR 绑定 `[ASSUMPTION]`）。令牌的展示/使用一律走 `--token-file` 或 stdin（`--token` 会进入 `ps`/shell 历史，仅作带警告的兜底，MIN-10）。Agent 以 **CA 指纹钉扎**（不信任系统根）连接 `:9444`，提交 CSR；Server 以单条 `UPDATE … WHERE used_at IS NULL AND expires_at > NOW(3)` **原子消费**；成功返回节点证书链、CA bundle、初始配置签名公钥集与节点参数。失败**统一**为 `enroll.token_invalid`（gRPC `NOT_FOUND`/`FAILED_PRECONDITION`，Tools 退出码 5；不区分不存在/已用/过期，故不存在 `enroll.token_used`）。
8. **授权**：拦截器由证书 SAN 得 `(role, uid)`，按角色→服务白名单放行；**每个请求里的资源 ID 必须属于该主体**（节点所在集群、证书、任务），否则 `NOT_FOUND`。
9. **服务划分**：`EnrollService`、`NodeControlService`（Heartbeat、RenewCertificate）、`RevisionService`（FetchRevision 流、FetchCertificate、ReportApply、GetSigningKeyset）、`TaskService`（PullTasks、ReportTask）、`TelemetryService`（UploadAccessLogs 流、GetLogCheckpoint、ReportRollups、ReportOriginHealth、ReportEvents）。**无任何执行命令/任意文件/任意 URL 的消息**（FR-35、NFR-14）。
10. **配置签名公钥集**：随 enroll 下发（信任起点），轮换用链式 keyset（见 ADR-012、node-protocol §8）。
11. **版本演进**：v1 内只做向后兼容变更；`buf breaking` 或等价工具做破坏检测（ADR-017）。
12. **CA 轮换（分阶段，Server 强制门禁；M1 文档化、人工驱动）**：旧方案“并存双 CA → 节点续期 → 撤旧”遗漏了关键约束——仅持旧 bundle 的节点无法验证新 CA 签发的 Server 叶证书，而新 bundle 只能经已认证通道取得，滞后/离线节点会被锁死。故分阶段：**S1** 新 CA 以 `signing_keys(mtls_ca, status=pending)` 加入 bundle，经新增 RPC `GetCaBundle` + Heartbeat 字段 `ca_bundle_version`/`latest_ca_bundle_version` + `DIRECTIVE_REFETCH_CA_BUNDLE` 下发（bundle 同时含新旧根）；**S2** 等待所有**证书未过期**的节点上报 `ca_bundle_version ≥ 新`（Server 记入 `nodes.ca_bundle_version`，`doctor` 对未追平节点告警）——**未满足不得进入 S3**；**S3** Server 叶证书与新签发的节点证书改由新 CA 签发；**S4** 节点经 `DIRECTIVE_RENEW_CERT` 续期换新 CA 证书；**S5** 无节点证书链至旧 CA 后撤旧 CA。离线节点在其证书过期前未追平则按 F-2 须重新 enroll。

## Binds / Prevents / Rule

- 约束：Server gRPC 层、Node Agent、后续 DNS/Reporter（同一 CA、不同 role）、Tools `node enroll`、Scheduler 的 `server.cert_rotate`。
- 防止：节点身份被冒用或 Token 重放；租户/普通 Token 触达节点接口；节点越权读取其他集群/租户的配置与证书；enroll 阶段中间人。
- 规则（AD-3）：**Node↔Server 仅 gRPC+mTLS，节点主动发起；身份=私有 CA 签发的证书（SAN 载荷 role+uid）；注册仅一次性 Token + CA 指纹钉扎；每个 RPC 校验资源归属。**

## 备选方案与权衡

- **共享密钥/HMAC 令牌作为节点身份**：否决——无法逐节点吊销与轮换，泄露即全局沦陷。
- **JWT bearer（Server 签发）**：否决——仍需传输层保护；mTLS 同时提供通道机密性与身份，且基线明确要求 mTLS。
- **长寿命证书（如 1–5 年）**：否决——吊销全靠服务端检查，长期私钥泄露窗口大；但短证书带来"离线过期需重新 enroll"的代价（F-2），采用 90 天折中，可被推翻。
- **Server 向节点推送（节点监听）**：否决——入站控制端口扩大攻击面，NAT 不友好。
- **enroll 使用 SSH/带外私钥分发**：否决——运维复杂；一次性 Token + 指纹钉扎已足够。

## 后果

- 节点长期离线（>90 天）需要人工重新注册；通过 `doctor`/告警缓解。
- CA 私钥泄露 = 全体节点身份可伪造（见威胁模型）；靠信封加密、仅签发时解密、`KeyProvider` 与备份规范缓解。
- CA 轮换需要分阶段门禁与 `GetCaBundle` 机制（原“并存双 CA”方案存在锁死窗口）；Server 叶证书的自动轮换成为新增的常驻任务。
- Enroll 监听端口"按需开启"增加一点实现复杂度，但减少常驻暴露面。

## 可被推翻处

证书有效期（90 天）与续期阈值；Token 是否绑定源 IP；若采用外部 KMS/硬件保存 CA 私钥，不影响协议。若要求"节点永不需要人工重新注册"，需引入更长证书有效期或离线续期机制（增加风险）。

## 验证

- 重放已用 Token 被拒；吊销证书后立即无法拉取配置；无证书连接在握手阶段被拒（FR-52 AC）。
- 用户 Token 调 gRPC、节点证书调 REST 均失败（FR-27 AC）。
- 越权资源请求返回 `NOT_FOUND`（拦截器单测 + 集成测试）。
- 证书续期与过期后重新 enroll 的验收脚本（真实节点；环境缺失标「等待环境验证」）。
- 续期中途失败注入：新证书落盘前 kill Agent / 丢弃响应，节点仍可用旧证书握手；新证书首次握手后旧证书进入 `superseded` 宽限。
- Server 叶证书轮换不中断存量连接；CA 轮换 S2 门禁：存在未追平节点时 S3 被拒。

## 修订记录 2026-11-16

- §1（MIN-07）：TLS 最低 1.3（threat-model §5.2 由“≥1.2”改为 1.3）。
- §3（IMP-05、M13）：新增 CA 有效期（10 年）、**Server 叶证书**（同一 CA 签发、serverAuth、SAN、`signing_keys.purpose='grpc_server'`、热重载、90 天自动轮换）；`ca_fp` 的定义。
- §4（IMP-05）：续期改**两阶段**（pending→首次握手后 active，旧证书 superseded + 宽限）；doctor 阈值 <1/6（L24）。
- §5/§7（M16、MIN-10）：重注册标志 `--force-reenroll`；enroll 失败统一 `enroll.token_invalid`；令牌绑定节点与 role，建议 `--token-file`/stdin。
- §12（IMP-05）：CA 轮换分 S1–S5 与 `GetCaBundle`/`ca_bundle_version` 门禁。原因：旧方案在 CA 轮换期会锁死仅持旧 bundle 的节点，续期非原子会使丢响应的节点只能人工重注册，Server 叶证书的签发者/SAN/私钥存放原文未定义。
