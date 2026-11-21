package main

import (
	"fmt"
	"io"
	"os"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

const usage = `用法: release <build|finalize|verify> [选项]（经 make release / release-finalize / release-verify 调用）

  build     构建、可复现性比对、组装 stage、产出第 1 轮待签清单 to-sign-1.json
  finalize  验证第 1 轮 .sig → 组装最终 .tar.gz → 生成待签 manifest.json 与 to-sign-2.json
  verify    验证 manifest.json.sig，并用 Tools 同一条校验链对每个包做真实验证；通过后产出 publish/ 发行目录

在线流水线不接触任何私钥；签名只在离线机用 scripts/release-sign 完成（docs/operations/release.md）。
退出码: 0 成功；1 失败；2 用法错误。
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "build":
		err = cmdBuild(args[1:], stdout, stderr)
	case "finalize":
		err = cmdFinalize(args[1:], stdout, stderr)
	case "verify":
		err = cmdVerify(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "未知子命令 %q\n%s", args[0], usage)
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "release %s 失败: %v\n", args[0], err)
		if _, ok := err.(usageError); ok {
			return 2
		}
		return 1
	}
	return 0
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }
