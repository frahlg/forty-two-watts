// Command genshipped writes the driver names drivers/BUNDLED_SOURCE.json pins.
package main

import (
	"fmt"
	"os"

	"github.com/srcfl/ftw/go/internal/fleetping/genshipped"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: genshipped <BUNDLED_SOURCE.json> <out.go>")
		os.Exit(2)
	}

	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	out, err := genshipped.Generate(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if err := os.WriteFile(os.Args[2], out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
