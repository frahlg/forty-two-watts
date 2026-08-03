---
"ftw": patch
---

Driver installs: the metadata check now reads the identity a signed artifact
was signed under, so a wrapped FTW-native source installs again.

A signed artifact assigns DRIVER three times when it wraps a source that
declares its own table: the generator's alias first, the source's inline
block, and the alias again at the end — reasserted exactly so the source
cannot overwrite the identity it was signed under. Lua runs the last
assignment, but the catalog parser preferred the inline block whenever one
existed. A v1.15.0 install refused the myuplink 1.1.1 beta with "driver
metadata id/version myuplink@1.0.0, want myuplink@1.1.1": the manifest and
the wrapper both said 1.1.1; the stale inline block said 1.0.0. The sungrow
artifact fails the same way on id — inline sungrow-shx against catalog
sungrow — so no wrapped FTW-native source could install from the repository
at all.

The parser now takes the assignment that appears last, as the VM would. A
trailing alias that names no parseable table still parses as empty rather
than borrowing an earlier table's identity, and an inline block written after
the trailing alias is reported as-is, so the installer refuses the override
against the manifest.
