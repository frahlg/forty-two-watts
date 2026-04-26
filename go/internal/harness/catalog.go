package harness

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/frahlg/forty-two-watts/go/internal/drivers"
)

// PrintCatalog walks a drivers directory, extracts each .lua file's DRIVER
// metadata block, and writes the result as one JSON array to w. Used by the
// `ftw-driver-harness catalog` subcommand (and eventually the GUI's driver
// picker) so operators don't have to cat .lua files to see what's available.
func PrintCatalog(w io.Writer, dir string) int {
	entries, err := drivers.LoadCatalog(dir)
	if err != nil {
		fmt.Fprintf(w, `{"error":%q}`+"\n", err.Error())
		return 1
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(entries); err != nil {
		fmt.Fprintf(w, `{"error":%q}`+"\n", err.Error())
		return 1
	}
	return 0
}
