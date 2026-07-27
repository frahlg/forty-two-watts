---
"ftw": minor
---

The driver editor is its own full-height view with syntax highlighting, line numbers, find and replace, and two linters. Ace is vendored under `/vendor/ace/` rather than pulled from a CDN, for the same reason three.js is — a gateway has to work without the internet, and a driver editor is most needed exactly when something is wrong. It loads on first open, not on page load. Ace's own Lua linter marks problems as you type; `POST /api/drivers/{id}/lint` then asks gopher-lua, the parser that actually decides whether the driver will start, and that verdict gates running a draft. Problems are listed under the editor and clicking one jumps to its line.
