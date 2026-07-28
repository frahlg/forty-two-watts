---
"ftw": patch
---

Core and the Optimizer now negotiate a protocol window instead of demanding an exact match. Each side declares the range of wire-protocol versions it speaks, and one shared version is enough — the same rule the driver host API already uses. Nothing changes today, because both sides speak exactly protocol 1; the point is that the next protocol change no longer has to land on both sides at the same moment. The contract is meant to move rarely: growing it means adding a feature to the handshake, which costs an older peer nothing, rather than bumping a version, which makes every peer outside the window incompatible at once.
