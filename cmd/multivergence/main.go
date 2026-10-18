// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

// Command multivergence reads and changes a Multivergence store file from
// the shell, so agents in any language can fork, weigh, constrain and
// promote branches of state. Run "multivergence help" for the commands.
package main

import (
	"os"

	"github.com/multivergence/multivergence/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
