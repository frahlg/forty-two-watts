---
"ftw": patch
---

A Core update now brings the updater sidecar with it. Core and the updater are built from the same release, but only Core was ever recreated, so the updater kept running whatever image it was installed with — and a fix inside the updater could not reach the sites that needed it, because the broken updater was the thing that had to run the fix. Getting out meant typing a compose command on each machine. The updater cannot recreate its own container without killing the command mid-flight, so it hands that last step to a short-lived detached container started from the image it is already running. This happens after Core is updated and verified healthy, and every failure leaves new Core plus the old updater — exactly where installs sit today, never worse.
