---
"ftw": minor
---

Edit a driver's Lua and try it for a fixed window. The edit runs as a local override, the driver restarts against it, and it puts the previous file back on its own unless you keep it — so the failure mode of walking away is the driver you started with. A draft that does not compile, or that renames itself into another driver's slot, never reaches the overlay; one that will not start is undone before the error is reported. Keeping a draft turns it into an ordinary override, which already shadows the channel and already tells you when a newer version exists.
