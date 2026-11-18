// Package migrationstest 是迁移框架的测试台（Story 1.6 AC5/AC6，`make migrate-test`）：
//
//   - ddl_rules_test.go：database/README §9 的 26 个 DDL 规则断言在真实 MySQL 上逐例执行，并与静态预检的判定逐例核对；
//   - skeleton_test.go：M1 迁移骨架（61 表 + 6 张后续阶段样例）在空库整体执行，及其按 13 个迁移文件经框架重放；
//   - framework_test.go：up/down/status、校验和篡改、dirty 与故障注入（中途失败、进程被杀）、互斥、DML 原子性、账号预检、逐表授权；
//   - lintfail_test.go：tests/fixtures/migrations/lint-fail 下每个负向用例必须恰好触发其 EXPECT 规则。
//
// 夹具只在 tests/fixtures/migrations；骨架与规则脚本（ddl-rules.sql、m1-fk-skeleton.sql、run_ddl_rules.py）是 docs/database/verification
// 的逐字节副本（用例与期望错误码不变；原件被 CF-1 冻结复核引用，不能搬走，TestRegressionAssetsAreIdenticalToFrozenDocs 守住两份一致）。
// MySQL 不可用时相应用例跳过并标「等待环境验证」（LINGEDGE_REQUIRE_MYSQL=1 时视为失败，make migrate-test 缺省如此）。
package migrationstest
