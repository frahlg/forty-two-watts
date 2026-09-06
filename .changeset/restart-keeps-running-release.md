---
"ftw": patch
---

Restart restarts the existing container and keeps its exact image, including local test builds. It never pulls or recreates from a stale Compose tag. Core refuses the unsafe restart path on older updaters and explains how to update the updater.
