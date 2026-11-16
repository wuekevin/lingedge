package main

import (
	"os"

	"github.com/wuekevin/lingedge/pkg/version"
)

func main() {
	os.Exit(version.CLI("lingedge", os.Args[1:], os.Stdout, os.Stderr))
}
