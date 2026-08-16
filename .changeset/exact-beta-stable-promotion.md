---
"ftw": patch
---

Promote the validated Core and updater beta images to stable without rebuilding them, while keeping the running release tag correct on both channels.

Core 2.0.0 and its updater are the components promoted by this release. The
optimizer keeps its independent release line:
`ghcr.io/srcfl/ftw-optimizer:latest` remains v1.3.2, while optimizer v1.4.0
remains on `ghcr.io/srcfl/ftw-optimizer:beta`.
