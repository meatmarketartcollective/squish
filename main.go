package main

import (
	"fmt"
	"os"

	"github.com/aloysiusvonfck/stackfold/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "stackfold:", err)
		os.Exit(1)
	}
}
