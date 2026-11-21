// Package probe 是 LingEdge Tools 的系统只读探测（Story 1.31、tools-design §10.4/§11、FR-6、FR-7）。
//
// 入口 Collect 产出 Result，其中 Result.System 是 resolver.System（Story 1.30 定义的输入契约，字段一一对应，没有另造一套），
// Result.Baota 是 baota.Report（写入 state.json 的 host.baota，经 registry.Registry.RecordHostBaota），
// Result.Sockets 带有 Listener 之外的全部占用进程（doctor 复用），Result.Warnings 记录降级与不可得项。
//
// 采集内容：OS/架构/内存/磁盘；本机监听（`ss -H -lntup` 解析，失败回退 /proc/net/{tcp,tcp6,udp,udp6} + /proc/<pid>/fd，
// 两者占用者信息都用 /proc 补全属主与 systemd 单元）；MySQL/Redis/Node.js 的存在性、版本、连通性；系统服务（named、systemd-resolved 等）；宝塔（baota 包）。
//
// 只读与安全：
//   - 进程创建只经 internal/tools/security/safeexec（本包的 Runner 白名单是 ss、systemctl、nginx、openresty、mysqld、mariadbd、redis-server、node，
//     固定目录解析、参数数组、清洗环境；systemctl 的参数还受本包的只读子命令表约束）；
//   - 不读取宝塔存储的 MySQL root 密码（baota.readGuard）；
//   - MySQL/Redis 连通性：先做免凭据探测（MySQL 只读取服务器握手包得到版本，不发送任何认证；Redis 只发 PING 与 INFO server），
//     调用方提供凭据时才认证并执行只读语句（SELECT VERSION() / INFO server）；凭据只存在于调用方传入的 Creds，从不写入 Result 或日志，错误经脱敏；
//   - 不代装任何依赖（T-2）：不可达只产生警告并引导到 Server 向导。
//
// 平台：采集器按 `//go:build linux` 隔离（statfs、/proc 默认根、mountinfo）；非 Linux 给出明确降级（os/arch 与可得项照常，其余在 Warnings 中注明），
// 仅用于开发机；生产目标是 Linux（amd64/arm64）。
package probe
