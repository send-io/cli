// Command sendio is send.io's official CLI.
package main

import (
	"os"

	"send.io/cli/internal/cli"
)

func main() { os.Exit(cli.Main()) }
