// Package signingkeys 实现信任域②③的密钥服务（Story 1.23，FR-26，ADR-002 §3/§11/§12、ADR-003 §3、ADR-012 §2）：
//
//   - Initialize：生成 Ed25519 配置签名密钥、ECDSA P-256 私有 CA（默认 10 年，可配 5–10 年）与由该 CA 签发的 Server gRPC 叶证书（serverAuth，
//     SAN = server_url 主机 + 设置项 grpc.tls.san_extra，90 天），私钥经 pkg/secretbox 信封加密（AAD=表|列|uid）入库；并初始化 system_settings(deploy.lineage)=1；
//     幂等、并发安全、与审计同一事务、缺主密钥时拒绝（入口供 Story 1.24 安装向导调用；本 Story 不注册 setup 子命令）；
//   - Sign：用 active 配置签名密钥对清单字节签名（域前缀 "LINGEDGE-REVISION-V1\n"、记录 key_id、不带过期时间，R-4），私钥仅在调用内解密并清零；
//     验证与链式 keyset 在 pkg/signing（节点侧复用，pkg 不依赖 internal）；
//   - GET /admin/system/signing-keys：只返回公钥与指纹，响应类型上不含私钥字段；
//   - CAStatus：CA 剩余有效期（< 1 年告警），供 `doctor`（Story 1.42）使用。
//
// 三个信任域（ADR-012 §2）：①发行清单签名私钥离线，**Server 代码路径中不存在任何加载发行私钥的逻辑**（门禁 `gates trustdomain` 与 tests/security/trustdomain 二进制符号检查）；
// ②③ 的私钥在本表，互相独立（CA 与配置签名密钥不共用，FR-26）。
//
// 不在本 Story：叶证书热重载与 server.cert_rotate、CA 轮换（Story 3.10）、keyset 轮换流程（Story 3.10）、setup 子命令（Story 1.24）。
package signingkeys
