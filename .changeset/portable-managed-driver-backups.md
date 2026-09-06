---
"ftw": patch
---

Full backups now include managed drivers whose active links use absolute paths inside the data directory. The archive stores relative links so restore works at a new path. Links that escape the data directory or form cycles remain blocked.
