package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func cmdMigrationsCheck(args []string) int {
	fl := flag.NewFlagSet("migrations-check", flag.ContinueOnError)
	dir := fl.String("dir", "migrations", "迁移目录")
	baseRef := fl.String("base-ref", "contract-v1", "基线 git ref（清单相对基线只追加）；空串跳过")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	var base []byte
	if *baseRef != "" {
		b, ok, err := gitShow(*baseRef, filepath.ToSlash(filepath.Join(*dir, checksumsName)))
		if err != nil {
			fmt.Fprintln(os.Stderr, "migrations-check 失败:", err)
			return 1
		}
		if ok {
			base = b
		}
	}
	fs, err := checkMigrations(*dir, base)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrations-check 失败:", err)
		return 2
	}
	if len(fs) > 0 {
		fmt.Fprintf(os.Stderr, "migrations-check 失败: %d 处违规\n", len(fs))
		printFindings(fs)
		return 1
	}
	raw, _ := os.ReadFile(filepath.Join(*dir, checksumsName))
	entries, _ := parseChecksums(string(raw))
	fmt.Printf("migrations-check OK: 已发布迁移 %d 个哈希一致，清单相对 %q 仅追加\n", len(entries), *baseRef)
	return 0
}

func cmdMigrationsSeal(args []string) int {
	fl := flag.NewFlagSet("migrations-seal", flag.ContinueOnError)
	dir := fl.String("dir", "migrations", "迁移目录")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	added, err := sealMigrations(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrations-seal 失败:", err)
		return 1
	}
	fmt.Printf("migrations-seal: 新登记 %d 个迁移 %v\n", len(added), added)
	return 0
}
