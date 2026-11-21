本目录由发行流水线（Story 1.34 的 make release，经 make build RELEASE=1）在构建前注入 keys.json（内置发行公钥集，go:embed all:builtin）。

- keys.json 不入库（.gitignore）：仓库内不存在任何发行公钥；真实公钥待发行签名人确认（UD-3）。
- 未注入 keys.json 的构建产物对任何清单都会以 VERIFY_FAILED 失败关闭（no_builtin_keys）；
  `make build RELEASE=1`在缺失或含测试公钥时直接构建失败（gates release-keys）。
- 测试私钥/公钥只存在于 tests/、_test.go、testdata/ 与测试替身构建标签的代码中，绝不进入本目录。
