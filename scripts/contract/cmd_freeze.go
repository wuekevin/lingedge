package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func cmdFreezeCheck(args []string) int {
	fl := flag.NewFlagSet("freeze-check", flag.ContinueOnError)
	root := fl.String("root", ".", "仓库根目录")
	m1Ops := fl.Int("m1-ops", 200, "期望的 M1 operation 数")
	m1Tables := fl.Int("m1-tables", 61, "期望的 M1 建表数")
	taskTypes := fl.Int("task-types", 25, "期望的 TaskType 枚举项数")
	reauth := fl.String("reauth", "scripts/contract/data/x-reauth.txt", "固定的 x-reauth operationId 清单（相对 root）")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	p := func(rel string) string { return filepath.Join(*root, rel) }
	read := func(rel string) string {
		b, err := os.ReadFile(p(rel))
		if err != nil {
			fmt.Fprintln(os.Stderr, "读取失败:", err)
			os.Exit(2)
		}
		return string(b)
	}

	data, err := os.ReadFile(p("docs/api/openapi.yaml"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取失败:", err)
		return 2
	}
	doc, err := loadDoc(data, "docs/api/openapi.yaml")
	if err != nil {
		fmt.Fprintln(os.Stderr, "freeze-check 失败（AC7a）:", err)
		return 1
	}
	pinned, err := readLines(p(*reauth))
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取 x-reauth 固定清单失败:", err)
		return 2
	}

	var fs []finding
	fs = append(fs, runAllChecks(doc, pinned)...)                                                                                              // AC7a：校验器、冒号、R-T1 及 AC8
	fs = append(fs, checkM1Operations(doc, *m1Ops)...)                                                                                         // AC7a：M1 = 200
	fs = append(fs, checkProtoSymbols(p("proto/lingedge/node/v1"))...)                                                                         // AC7b（编译/lint 由 make proto-check 的 buf 完成）
	fs = append(fs, checkMigrationOrder(read("docs/database/README.md"), read("docs/database/verification/m1-fk-skeleton.sql"), *m1Tables)...) // AC7c
	fs = append(fs, checkEnvPendingRegistered(read("docs/bmad/epics.md"))...)                                                                  // AC7d
	fs = append(fs, checkCF1Record(p("docs/bmad/contract-changes/CF-1.md"))...)                                                                // AC5/AC7e

	docs := map[string]string{}
	for _, pat := range []string{"docs/database/*.md", "docs/api/*.md", "docs/architecture/*.md", "docs/architecture/adr/*.md"} {
		ms, _ := filepath.Glob(p(pat))
		for _, m := range ms {
			b, rerr := os.ReadFile(m)
			if rerr != nil {
				fmt.Fprintln(os.Stderr, "读取失败:", rerr)
				return 2
			}
			rel, _ := filepath.Rel(*root, m)
			docs[rel] = string(b)
		}
	}
	fs = append(fs, checkTaskTypeScope(doc, *taskTypes, docs)...) // AC9

	if len(fs) > 0 {
		fmt.Fprintf(os.Stderr, "freeze-check 失败: %d 处不成立；不得打 contract-v1\n", len(fs))
		printFindings(fs)
		return 1
	}
	st := statsOf(doc)
	fmt.Printf("freeze-check OK: M1 operation %d，路径含冒号 0，R-T1 泄漏 0；proto 符号齐全；迁移顺序与骨架一致（M1 %d 表）；5 项待环境验证已登记；CF-1 记录引用修订日志；TaskType %d 项且不含内部周期作业（operation 共 %d）\n",
		*m1Ops, *m1Tables, *taskTypes, st.Ops)
	return 0
}
