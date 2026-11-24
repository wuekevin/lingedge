package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/wuekevin/lingedge/internal/platform/config"
)

// configMain 实现 `lingedge-server config check [--config FILE] [--json]`（Story 1.42）：用与启动时完全相同的 config.Parse 校验 server.yaml
// （未知键/重复键/类型、监听地址、metrics 回环、口令不得写在文件里、口令文件权限……），只做取值校验，不要求运行期必需项（setup mode 的配置本来就不含数据库连接）。
// 它是 Tools `config validate server` 与 `doctor cfg.validate` 的校验来源——Tools 不 import Server 的 internal 包（architecture §5.3 规则 5），
// 经子命令协作，所以规则永远只有一份。输出只含键路径与规则说明，永不回显任何取值。
// 退出码：0 通过；1 有问题（--json 时 stdout 为 {"ok":false,"problems":[{"path","reason"}]}）；2 用法错误。
// 环境变量覆盖（LINGEDGE_*）按进程环境生效，与 Server 启动一致；Tools 以清空的环境调用，所以那一路校验的是文件本身。
func configMain(args []string, stdout, stderr io.Writer, environ []string) int {
	if len(args) == 0 || args[0] != "check" {
		_, _ = fmt.Fprintf(stderr, "config: 用法：lingedge-server config check [--config FILE] [--json]\n")
		return 2
	}
	fl := flag.NewFlagSet("config check", flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfgPath := fl.String("config", "", "server.yaml 路径（缺省 $"+EnvConfig+"，再缺省 "+DefaultConfigPath+"）")
	asJSON := fl.Bool("json", false, "stdout 输出 JSON")
	if err := fl.Parse(args[1:]); err != nil {
		return 2
	}
	if fl.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "config check: 多余的参数\n")
		return 2
	}
	_, err := loadConfig(*cfgPath, environ, false)
	if err == nil {
		if *asJSON {
			_, _ = fmt.Fprintln(stdout, `{"ok":true,"problems":[]}`)
		} else {
			_, _ = fmt.Fprintln(stdout, "配置通过校验")
		}
		return 0
	}
	type problem struct {
		Path   string `json:"path"`
		Reason string `json:"reason"`
	}
	var probs []problem
	var ce *config.Error
	if errors.As(err, &ce) {
		for _, p := range ce.Problems {
			probs = append(probs, problem{Path: p.Path, Reason: p.Reason})
		}
	} else {
		probs = append(probs, problem{Reason: err.Error()})
	}
	if *asJSON {
		b, _ := json.Marshal(map[string]any{"ok": false, "problems": probs})
		_, _ = fmt.Fprintln(stdout, string(b))
	} else {
		for _, p := range probs {
			if p.Path != "" {
				_, _ = fmt.Fprintf(stdout, "%s: %s\n", p.Path, p.Reason)
			} else {
				_, _ = fmt.Fprintf(stdout, "%s\n", p.Reason)
			}
		}
	}
	return 1
}
