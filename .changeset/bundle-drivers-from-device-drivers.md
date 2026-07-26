---
"ftw": patch
---

The bundled `drivers/` tree is now a generated snapshot of srcfl/device-drivers, pinned by commit, with CI failing if the two drift. It had become a second source of truth and had already diverged: a Sungrow fix landed upstream while the bundled copy kept reading a battery block on inverters that have no battery, which is what took a customer's SG12RT offline. That copy is now correct, and cannot silently fall behind again.
