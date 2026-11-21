// Package baota 是 LingEdge Tools 的宝塔与既有 Web/数据库环境只读探测（Story 1.31、tools-design §11、FR-7）。
//
// 只读是结构性的：
//   - 所有文件访问都经 FS 接口（默认 OSFS，限定在一个根目录之下，不跟随符号链接），且 ReadFile 经 readGuard——宝塔存储的 MySQL root 密码
//     （面板 SQLite default.db、default.pl 等）、redis.conf、my.cnf、私钥证书一律拒绝打开，哪怕调用方想读；
//   - 本包不创建任何进程；版本号需要执行 `nginx -v` 之类时，由调用方经 internal/tools/security/safeexec 完成并通过 Options.Exec 传入
//     （见 internal/tools/probe）；
//   - 本包不写入任何文件。
//
// 输出 Report 可 JSON 序列化，写入 state.json 的 host.baota（internal/tools/registry），也被 probe（产出 resolver.System）与未来 doctor（1.42）复用。
//
// 判定规则：宝塔面板有三类证据——面板目录 /www/server/panel、bt 命令（/usr/bin/bt 或 /etc/init.d/bt）、面板端口文件 data/port.pl；
// 任两类成立才判 detected（high=三类，medium=两类）；只有一类时不判定，仅在 Evidence 里列出并给出说明。
// 宝塔存在不阻止安装：只有真实冲突（端口/路径/同名）才由 Assess 报告为阻断。
//
// 默认路径是宝塔常见布局，来自文档与经验，真实宝塔机上的复验属 ENV-BT（见 tests/fixtures/baota/README.md）。
package baota
