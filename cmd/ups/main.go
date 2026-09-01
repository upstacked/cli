// Command ups is the Upstacked CLI.
package main

import (
	"os"

	"github.com/upstacked/cli/internal/cli"
)

// version is set at build time with -ldflags.
var version = "dev"

func main() {
	os.Exit(cli.Execute(version))
}
