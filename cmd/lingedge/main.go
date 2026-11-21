package main

import (
	"os"

	"github.com/wuekevin/lingedge/internal/tools/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
