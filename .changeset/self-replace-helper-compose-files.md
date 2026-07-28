---
"ftw": patch
---

Fix the sidecar replacement introduced last release: it handed the detached helper a compose file the helper could not read. The updater builds its compose command from the merged config, which during a Core update includes a transient override in the updater's own `/tmp`. The helper is a separate container starting three seconds later, so that path is neither in its mount namespace nor still on disk. The helper now uses only the files that exist on the host, and so does the matching pull, so both resolve the same image. Caught on a Raspberry Pi running the real update path; the Core update itself completed and stayed healthy throughout, exactly as the best-effort design intended.
