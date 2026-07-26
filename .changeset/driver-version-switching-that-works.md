---
"ftw": minor
---

Switch driver versions, and switch back when the new one misbehaves. Nothing from the signed channel could be installed at all: the metadata parser only read a DRIVER table written inline, and signed artifacts assign it from a local, so every install failed validation with an empty id and version. The Versions list rendered no rows because it read the version off the wrong object, and its install call omitted the repository id the API requires. Going back to the bundled driver was impossible once a channel version was installed — the bundled copy is not an install, so nothing could activate it — which is the case that matters most, since installing over the bundled driver is the first thing anyone does. Each driver row now says which version runs, whether it came from the channel or your own file, and how well tested it is; the version list offers one click to switch, a standing way back to the copy that shipped with the build, and an undo after a switch.
