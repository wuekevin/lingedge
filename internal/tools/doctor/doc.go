// Package doctor 是 `lingedge doctor` 的检查项注册表与内置检查（Story 1.42，tools-design §19，FR-15）。
//
// 结构：
//   - Registry 持有若干 Group（一组同前缀的检查项，如 sys.*、port.*）。每个 Group 的 Run 读取 World（本机事实的只读视图，全部可注入）并返回
//     Finding（稳定的 id/category、pass|warn|fail|skip、message、hint）。注册表校验 id 的形态、前缀归属与唯一性，所以后续 Story
//     （Epic 3 的 node.*/health.node、4.x 的 dns.*/disk.*）只需 Register 自己的 Group，不改本包的框架。
//   - 检查项只读：除 `--fix-perms`（FixPerms，仅修复 master.key/config/identity 的明确偏差，逐项输出）外不改变任何东西；
//     没有 root 权限读不了的东西报告 skip 并说明原因，而不是猜测。
//   - 输出经脱敏：Finding 的 message/hint/details 一律过 security 脱敏，检查本身也从不读取密钥内容（只看路径、权限、属主、指纹由调用方给出）。
//
// 本包不 import cli（cli 是消费者）；与 Server 的协作只经调用方注入的函数（`lingedge-server config check`、HTTP /healthz）。
package doctor
