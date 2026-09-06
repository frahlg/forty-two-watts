---
"ftw": patch
---

Stop savings database reads when an app request times out or is canceled, so the request releases its place for later app reads. Keep canceled calculations out of the daily savings cache.
