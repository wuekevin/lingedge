package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"io"
	"os"
	"strings"
)

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

const usage = `用法: release-sign <子命令> [选项]（仅在离线机运行；发行私钥永不进入仓库/CI）

  keygen -key-id ID -key-out FILE [-pub-out FILE] [-not-before T] [-not-after T]
        生成密钥对；私钥 0600、不覆盖；公钥条目（keys.json 格式）写 -pub-out 或标准输出
  pubkey -key FILE
        由私钥文件导出公钥条目
  sign   -key FILE -to-sign DIR/to-sign-N.json [-expect-digest HEX] [-yes] [-force]
        核对并（人工确认后）签名待签清单里的全部对象，输出 .sig
  verify -keys keys.json -to-sign DIR/to-sign-N.json
        用公钥集复核已产出的 .sig
  endorse -key OLD私钥 -new-keys 新公钥.json [-out FILE]
        公钥轮换：用旧 active 私钥为新公钥背书，输出清单的 key_rotation 段（finalize -key-rotation 使用）

退出码: 0 成功；1 失败（核对/签名/复核不通过）；2 用法错误。
`

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fprintf(stderr, "%s", usage)
		return 2
	}
	fl := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fl.SetOutput(stderr)
	switch args[0] {
	case "keygen":
		id := fl.String("key-id", "", "密钥标识，如 rel-2026-a")
		out := fl.String("key-out", "", "私钥文件输出路径（离线机上的受保护目录）")
		pub := fl.String("pub-out", "", "公钥条目输出路径（缺省标准输出）")
		nb := fl.String("not-before", "", "公钥有效期起（RFC 3339，可选）")
		na := fl.String("not-after", "", "公钥有效期止（RFC 3339，可选）")
		if fl.Parse(args[1:]) != nil {
			return 2
		}
		if *id == "" || *out == "" {
			fprintf(stderr, "keygen 需要 -key-id 与 -key-out\n")
			return 2
		}
		e, err := generateKey(*id, *out, *nb, *na)
		if err != nil {
			fprintf(stderr, "keygen 失败: %v\n", err)
			return 1
		}
		if err := writePub(e, *pub, stdout); err != nil {
			fprintf(stderr, "写公钥失败: %v\n", err)
			return 1
		}
		fprintf(stderr, "已生成私钥 %s（0600）。把它保管在离线介质上，绝不要放进仓库或 CI；公钥条目并入 keys.json 后随 Tools 发行。\n", *out)
		return 0
	case "pubkey":
		key := fl.String("key", "", "私钥文件")
		if fl.Parse(args[1:]) != nil || *key == "" {
			return 2
		}
		s, err := loadKey(*key)
		if err != nil {
			fprintf(stderr, "%v\n", err)
			return 1
		}
		if err := writePub(s.entry("", ""), "", stdout); err != nil {
			return 1
		}
		return 0
	case "sign":
		key := fl.String("key", "", "私钥文件")
		list := fl.String("to-sign", "", "待签清单 to-sign-N.json")
		expect := fl.String("expect-digest", "", "待签清单文件的 SHA-256（与在线流水线控制台/带外渠道所示一致，防止被调包）")
		yes := fl.Bool("yes", false, "跳过交互确认（仅用于自动化演练；真实发行必须人工确认）")
		force := fl.Bool("force", false, "覆盖已存在的 .sig")
		if fl.Parse(args[1:]) != nil || *key == "" || *list == "" {
			fprintf(stderr, "sign 需要 -key 与 -to-sign\n")
			return 2
		}
		return cmdSign(*key, *list, *expect, *yes, *force, stdin, stderr)
	case "verify":
		keys := fl.String("keys", "", "keys.json")
		list := fl.String("to-sign", "", "待签清单")
		if fl.Parse(args[1:]) != nil || *keys == "" || *list == "" {
			return 2
		}
		ts, dir, _, err := loadToSign(*list)
		if err != nil {
			fprintf(stderr, "%v\n", err)
			return 1
		}
		n, err := verifyAll(*keys, ts, dir)
		if err != nil {
			fprintf(stderr, "复核失败: %v\n", err)
			return 1
		}
		fprintf(stdout, "复核通过：%d 个签名\n", n)
		return 0
	case "endorse":
		key := fl.String("key", "", "旧 active 私钥文件")
		nk := fl.String("new-keys", "", "新公钥（keys.json 格式，由 keygen 产出）")
		outp := fl.String("out", "", "输出文件（缺省标准输出）")
		if fl.Parse(args[1:]) != nil || *key == "" || *nk == "" {
			fprintf(stderr, "endorse 需要 -key 与 -new-keys\n")
			return 2
		}
		s, err := loadKey(*key)
		if err != nil {
			fprintf(stderr, "%v\n", err)
			return 1
		}
		kr, err := endorse(s, *nk)
		if err != nil {
			fprintf(stderr, "endorse 失败: %v\n", err)
			return 1
		}
		b, _ := json.MarshalIndent(kr, "", "  ")
		b = append(b, '\n')
		if *outp == "" {
			_, _ = stdout.Write(b)
		} else if err := os.WriteFile(*outp, b, 0o644); err != nil {
			fprintf(stderr, "%v\n", err)
			return 1
		}
		return 0
	case "help", "-h", "--help":
		fprintf(stdout, "%s", usage)
		return 0
	}
	fprintf(stderr, "未知子命令 %q\n%s", args[0], usage)
	return 2
}

func writePub(e *pubEntry, path string, stdout io.Writer) error {
	doc := keysDoc{SchemaVersion: 1, Keys: []pubEntry{*e}, Revoked: []string{}}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if path == "" {
		_, err = stdout.Write(b)
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func cmdSign(keyPath, listPath, expect string, yes, force bool, stdin io.Reader, stderr io.Writer) int {
	s, err := loadKey(keyPath)
	if err != nil {
		fprintf(stderr, "%v\n", err)
		return 1
	}
	ts, dir, digest, err := loadToSign(listPath)
	if err != nil {
		fprintf(stderr, "%v\n", err)
		return 1
	}
	if expect != "" && !strings.EqualFold(expect, digest) {
		fprintf(stderr, "待签清单摘要不符：文件 sha256=%s，带外给出 %s。拒绝签名。\n", digest, expect)
		return 1
	}
	ps, err := prepare(ts, dir)
	if err != nil {
		fprintf(stderr, "核对未通过，拒绝签名: %v\n", err)
		return 1
	}
	fprintf(stderr, "== 待签清单 %s（发行 %s，提交 %s，第 %d 轮）\n   清单文件 sha256=%s\n   签名密钥 key_id=%s\n", listPath, ts.Release, ts.Commit, ts.Round, digest, s.keyID)
	for _, p := range ps {
		for _, l := range p.lines {
			fprintf(stderr, "   %s\n", l)
		}
	}
	if !yes {
		if !isTerminal(os.Stdin) && stdin == os.Stdin {
			fprintf(stderr, "标准输入不是终端：真实签名需要人工确认；自动化演练请显式加 -yes。\n")
			return 1
		}
		fprintf(stderr, "核对以上内容无误后输入 sign 确认: ")
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		if strings.TrimSpace(line) != "sign" {
			fprintf(stderr, "未确认，已取消。\n")
			return 1
		}
	}
	if err := writeSigs(s, ps, force); err != nil {
		fprintf(stderr, "写签名失败: %v\n", err)
		return 1
	}
	fprintf(stderr, "已签名 %d 个对象。把 .sig 文件带回在线机继续流水线（私钥不离开本机）。\n", len(ps))
	return 0
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
