---
"ftw": patch
---

The energy flow's house hub is clickable, like its planets. It carries `data-role="load"` and fires the same `ftw-planet-click` event, so a host can open the house's own live reading from a tap on the centre — the app draws a live line for it, the way it already does for grid, solar and battery. Purely additive: nothing that ignores the event changes.
