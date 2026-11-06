package main

import (
	"fmt"
	"os"

	"github.com/aloysiusvonfck/squish/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "squish:", err)
		os.Exit(1)
	}
}
