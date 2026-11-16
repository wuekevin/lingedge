// Package dbintegration 是 Story 1.5 的数据库层集成测试：真实 MySQL 8.0（独立临时实例，见 tests/mysqltest），无 Mock。
// 覆盖：连接会话（UTC/utf8mb4/parseTime/参数化）、事务传播、租户 Scope 越权负向矩阵、三类账号权限负向。
// 无 MySQL 的环境（CI）干净地跳过并标「等待环境验证」，不会假装通过。
package dbintegration
