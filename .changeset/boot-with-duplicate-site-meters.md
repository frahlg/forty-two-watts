---
"ftw": patch
---

A config with two `is_site_meter: true` drivers no longer stops the box from starting. The box boots with the first declared driver as the site meter — the same one older versions silently used — ignores the flag on the rest, and logs a clear error naming both drivers so the mistake is visible in the log and the help report. Saving such a config from Settings is still rejected. A driver install that accidentally added a second site meter used to crash-loop the box before the web UI came up, leaving SSH as the only way back in.
