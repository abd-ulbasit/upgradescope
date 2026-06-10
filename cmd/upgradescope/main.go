package main

import (
	"fmt"
	"os"

	"github.com/abd-ulbasit/upgradescope/internal/cli"
)

func main() {
	if err := cli.Root().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(cli.ExitCode(err)) // 2 = --fail-on threshold hit, 1 = error
	}
}
