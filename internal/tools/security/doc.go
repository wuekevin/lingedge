// Package security 是 LingEdge Tools 的安全叶子包（Story 1.26、tools-design §18、FR-9、NFR-9/11/12）。
//
// 所有归档解压、外部命令、日志脱敏与文件权限都经这里的受控入口，使路径穿越、命令注入与口令泄露在结构上被杜绝：
//   - SafeExtract（extract.go）：只允许普通文件/目录，拒绝链接/设备/FIFO，路径与落点双重校验，限额对声明与实际大小都检查，
//     边写边校验 SHA256，失败整体删除 .partial（G-4）；
//   - safeexec 子包：唯一允许创建进程的位置（forbidigo 白名单，G-12），仅参数数组、可执行文件白名单、环境清洗；
//   - ValidateUnitValue（unit.go）：写入 systemd unit 等生成文件的值的字符集校验；
//   - 脱敏（redact.go）：对 pkg/redact 的封装，不另立规则（NFR-12、G-5）；
//   - 权限助手（perm.go）：umask 077 + 显式 chmod，创建即最终权限。
//
// 本包是叶子：只依赖标准库与 pkg/redact，不 import 任何 internal 包。
// 平台相关代码按 //go:build 隔离（platform_unix.go / platform_other.go）；Linux 专属行为在 safeexec 子包。
package security
