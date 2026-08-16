---
"ftw": patch
---

Core now cancels blocked Lua work, read-only HTTP, or sleep before it runs the driver's autonomous default. Mutating HTTP stays ordered until the host transport returns, and a dedicated default queue keeps the safety request ahead of stale control commands without calling one driver in parallel.
