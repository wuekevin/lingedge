// Package compilefail 用真实编译器断言「必须编译失败」的 API 误用（Story 1.5 AC2/AC3/AC6 的编译期保证）：
// testdata 下每个包是一种违规写法，ok 包是正向对照。这些包位于 testdata，不参与 `go build ./...`，只被本测试显式编译。
package compilefail
