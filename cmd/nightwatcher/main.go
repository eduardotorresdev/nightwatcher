// Command nightwatcher is both the daemon and the operating tool: `serve` raises the
// daemon, every other subcommand is run against the same image by `dokku run`.
package main

import (
	"fmt"
	"os"

	"github.com/eduardotorresdev/nightwatcher/internal/cli"
)

func main() {
	if err := cli.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "nightwatcher:", err)
		os.Exit(1)
	}
}
