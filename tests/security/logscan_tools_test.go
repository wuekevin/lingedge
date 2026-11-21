package security_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/cli"
)

// G-5（Tools 场景，Story 1.25 AC6）：经 lingedge 命令行传入的令牌/口令/DSN 口令，在 JSONL 日志、stdout 的 envelope、stderr 的人类输出与错误信息中
// 都不得出现。注入值运行期随机生成（G-6：无固定口令）。命令走真实的 cli.Execute（真实命令树、校验与日志文件），
// 以 root 身份通过 root 检查，使参数完整经过「校验 → 确认 → 尚未实现」各阶段。
func TestLogScanToolsCLI(t *testing.T) {
	gen := func(prefix string) string {
		b := make([]byte, 18)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		return prefix + base64.RawURLEncoding.EncodeToString(b)
	}
	enrollTok, apiTok, dbPW, urlPW, adminPW := "lenr1."+gen("e"), "le_live_"+gen("a"), gen("db-"), gen("url-"), gen("adm-")
	// 无任何已知形态的不透明值：只有「按标志名打码」这一层能保护它们（redactArgs），值格式识别帮不上忙。
	opaqueTok, opaquePW := strings.ToLower(gen("")), strings.ToLower(gen(""))
	pwKey, adminKey := "database."+"pass"+"word", "admin_"+"pass"+"word"
	secrets := []string{enrollTok, apiTok, dbPW, urlPW, adminPW, opaqueTok, opaquePW}

	dir := t.TempDir()
	logf := filepath.Join(dir, "tools.log")
	var all bytes.Buffer
	run := func(asRoot bool, args ...string) {
		var out, errb bytes.Buffer
		env := cli.DefaultEnv()
		env.Stdout, env.Stderr = &out, &errb
		env.DefaultLogFile = ""
		env.StatePath = filepath.Join(dir, "state.json")
		if asRoot {
			env.Geteuid = func() int { return 0 }
		}
		cli.Execute(context.Background(), env, append(args, "--log-file", logf, "--log-level", "debug"))
		all.Write(out.Bytes())
		all.Write(errb.Bytes())
	}
	// 令牌：两种写法 + 文件路径形态；非 root 路径（sudo 提示里不得回显）与 root 路径各一遍。
	for _, root := range []bool{false, true} {
		run(root, "node", "enroll", "--token", enrollTok, "--server", "https://panel.example.com", "--json")
		run(root, "node", "enroll", "--token="+enrollTok, "--force-reenroll", "--yes")
		run(root, "node", "enroll", "--server", "https://admin:"+urlPW+"@panel.example.com", "--token", enrollTok) // 非法 URL（内嵌口令）→ 用法错误
		run(root, "install", "server", "--set", pwKey+"="+dbPW, "--set", adminKey+"="+adminPW, "--set", "api_token="+apiTok, "--json")
		run(root, "install", "server", "--set="+"db_dsn=lingedge_app:"+dbPW+"@tcp(127.0.0.1:3306)/lingedge")
		run(root, "node", "enroll", "--token", opaqueTok, "--json")
		run(root, "node", "enroll", "--token="+opaqueTok)
		run(root, "install", "server", "--set", pwKey+"="+opaquePW)
		run(root, "install", dbPW, "--token", apiTok) // 组件名即口令（用户输错参数）→ 用法错误，回显前经脱敏
	}
	raw, err := os.ReadFile(logf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "***") {
		t.Fatal("日志里没有脱敏标记，测试可能没有真正记录命令参数")
	}
	for _, s := range secrets {
		if bytes.Contains(raw, []byte(s)) {
			t.Errorf("日志泄露了 %.10s…\n%s", s, raw)
		}
		if bytes.Contains(all.Bytes(), []byte(s)) {
			t.Errorf("stdout/stderr 泄露了 %.10s…", s)
		}
	}
}
