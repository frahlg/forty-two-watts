// Command gencontract writes the Go constants for contract/registry.yaml.
package main

import (
	"fmt"
	"os"

	"github.com/srcfl/ftw/go/internal/appproto/gencontract"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: gencontract <proto|roles> <registry.yaml> <out.go>")
		os.Exit(2)
	}

	raw, err := os.ReadFile(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var out []byte
	switch os.Args[1] {
	case "proto":
		out, err = gencontract.Generate(raw)
	case "roles":
		out, err = gencontract.GenerateRoles(raw)
	default:
		fmt.Fprintf(os.Stderr, "unknown target %q\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if err := os.WriteFile(os.Args[3], out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
