package mpc

// PowerLimits caps the grid flow the DP is allowed to consider in a
// given slot. Default (zero value) is unlimited — the MPC plans as if
// the grid connection has unbounded capacity in both directions.
//
// Use cases:
//
//   - Dynamic-capacity tariff: "between 17-20 local, max 5 kW import"
//     (the tariff charges a fixed monthly fee for every kW peak above
//     that) — set MaxImportW = 5000 on affected slots.
//
//   - DSO / grid operator curtailment signal: "zero export during
//     congestion event" — set MaxExportW to a small value. The DP
//     then avoids plans that require exporting more than that.
//
//   - Phase-current limit from a constrained service entrance: cap
//     import so battery charging + household load don't trip the
//     main fuse even when prices would make grid-charging attractive.
//
// Inspired by the per-slot PowerLimits dataclass in
// srcful-nova-ems-x; kept as a plain value (not a pointer) so the
// zero-value default is "no limit applied" without any nil-check
// boilerplate at call sites.
type PowerLimits struct {
	// MaxImportW caps positive grid flow (site convention: grid W > 0
	// = importing). A value of 0 or negative means unlimited.
	MaxImportW float64

	// MaxExportW caps the magnitude of negative grid flow
	// (|grid W| when grid W < 0). A value of 0 or negative means
	// unlimited.
	MaxExportW float64
}

// allowsImport reports whether the slot permits the given positive
// grid flow (W). Returns true for any non-positive flow (import check
// only applies to import).
func (l PowerLimits) allowsImport(gridW float64) bool {
	if gridW <= 0 || l.MaxImportW <= 0 {
		return true
	}
	return gridW <= l.MaxImportW
}

// allowsExport reports whether the slot permits the given negative
// grid flow (W). Returns true for any non-negative flow (export check
// only applies to export).
func (l PowerLimits) allowsExport(gridW float64) bool {
	if gridW >= 0 || l.MaxExportW <= 0 {
		return true
	}
	return -gridW <= l.MaxExportW
}
