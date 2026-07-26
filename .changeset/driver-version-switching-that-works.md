---
"ftw": minor
---

Switch driver versions, and switch back when the new one misbehaves. Nothing from the signed channel could be installed at all: the metadata parser only read a DRIVER table written inline, and signed artifacts assign it from a local, so every install failed validation with an empty id and version. The Versions list rendered no rows because it read the version off the wrong object, and its install call omitted the repository id the API requires. Rolling back the first install over a bundled driver was refused outright, which is the case that matters most — installing over the bundled driver is the first thing anyone does. Each driver row now says which version runs, whether it came from the channel or your own file, and how well tested it is; the version list offers one click to switch and an undo to put the old one back.
