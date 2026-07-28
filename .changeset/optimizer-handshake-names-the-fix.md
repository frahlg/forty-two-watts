---
"ftw": patch
---

When Core rejects the Optimizer's handshake it now says the Optimizer is too old and to update it in Update Center, instead of naming an internal feature flag. This failure is silent by construction: a Core update never moves Optimizer, and the Optimizer container validates its own handshake against its own constants — so an image older than the champion solver reports itself healthy to Docker and to the updater while Core quietly refuses it and plans on the Go fallback. The rejection string is the only thing an operator ever sees, so it has to name the fix.
