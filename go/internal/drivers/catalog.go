package drivers

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// CatalogEntry describes one available driver discovered in the drivers
// directory. Populated from the DRIVER={…} table each .lua file declares
// at the top. Missing fields are left empty.
type CatalogEntry struct {
	Path               string         `json:"path"`          // relative to config dir
	Filename           string         `json:"filename"`      // e.g. "ferroamp.lua"
	ID                 string         `json:"id"`
	Name               string         `json:"name"`
	Manufacturer       string         `json:"manufacturer,omitempty"`
	Version            string         `json:"version,omitempty"`
	Protocols          []string       `json:"protocols,omitempty"`            // mqtt / modbus / http
	Capabilities       []string       `json:"capabilities,omitempty"`         // meter / pv / battery
	Description        string         `json:"description,omitempty"`
	Homepage           string         `json:"homepage,omitempty"`
	ConnectionDefaults map[string]any `json:"connection_defaults,omitempty"`
}

// LoadCatalog scans dir (and any direct sub-directories) for .lua driver
// files and extracts their DRIVER metadata table. Files without a DRIVER
// block are still returned — just with ID/Name empty — so operators can
// at least see they exist.
func LoadCatalog(dir string) ([]CatalogEntry, error) {
	var out []CatalogEntry
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable, don't fail the whole scan
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".lua") {
			return nil
		}
		entry, err := parseCatalogEntry(path)
		if err != nil {
			return nil // skip malformed
		}
		rel, _ := filepath.Rel(dir, path)
		entry.Path = filepath.Join(filepath.Base(dir), rel)
		entry.Filename = filepath.Base(path)
		out = append(out, entry)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", dir, err)
	}
	// Stable sort by name (then filename as tiebreaker).
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Name, out[j].Name
		if a == b {
			return out[i].Filename < out[j].Filename
		}
		if a == "" {
			return false
		}
		if b == "" {
			return true
		}
		return a < b
	})
	return out, nil
}

// parseCatalogEntry opens the .lua file, finds the DRIVER = {…} block,
// and extracts string/list fields via regex. Lightweight — no Lua VM.
func parseCatalogEntry(path string) (CatalogEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CatalogEntry{}, err
	}
	s := string(data)
	block := extractDriverBlock(s)
	e := CatalogEntry{}
	e.ID = pickString(block, "id")
	e.Name = pickString(block, "name")
	e.Manufacturer = pickString(block, "manufacturer")
	e.Version = pickString(block, "version")
	e.Description = pickString(block, "description")
	e.Homepage = pickString(block, "homepage")
	e.Protocols = pickList(block, "protocols")
	e.Capabilities = pickList(block, "capabilities")
	e.ConnectionDefaults = pickKVBlock(block, "connection_defaults")
	return e, nil
}

var driverBlockRe = regexp.MustCompile(`(?s)DRIVER\s*=\s*\{(.*?)\n\}`)

func extractDriverBlock(src string) string {
	m := driverBlockRe.FindStringSubmatch(src)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// pickString matches `name = "value"` inside the block.
func pickString(block, name string) string {
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(name) + `\s*=\s*"([^"]*)"`)
	m := re.FindStringSubmatch(block)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// pickList matches `name = { "a", "b", "c" }` inside the block.
func pickList(block, name string) []string {
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(name) + `\s*=\s*\{([^}]*)\}`)
	m := re.FindStringSubmatch(block)
	if len(m) < 2 {
		return nil
	}
	items := regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[1], -1)
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it[1])
	}
	return out
}

// kvPairRe matches `key = "string"` or `key = 123` inside a Lua table body.
var kvPairRe = regexp.MustCompile(`(\w+)\s*=\s*(?:"([^"]*)"|([^\s,]+))`)

// pickKVBlock matches a nested Lua table `name = { key = val, ... }`
// and returns key-value pairs as a map. Values can be numbers or quoted
// strings. Returns nil when the block is absent.
func pickKVBlock(block, name string) map[string]any {
	re := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(name) + `\s*=\s*\{([^}]*)\}`)
	m := re.FindStringSubmatch(block)
	if len(m) < 2 {
		return nil
	}
	pairs := kvPairRe.FindAllStringSubmatch(m[1], -1)
	if len(pairs) == 0 {
		return nil
	}
	out := make(map[string]any, len(pairs))
	for _, p := range pairs {
		key := p[1]
		if p[2] != "" {
			out[key] = p[2]
		} else if f, err := strconv.ParseFloat(p[3], 64); err == nil {
			if f == float64(int64(f)) {
				out[key] = int64(f)
			} else {
				out[key] = f
			}
		} else {
			out[key] = p[3]
		}
	}
	return out
}
