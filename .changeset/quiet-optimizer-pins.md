---
"ftw": patch
---

Keep the selected optimizer image after updates and rollbacks by saving and checking its Compose pin. Report a failed pin write instead of a successful update, preserve other host settings and file permissions, and keep shell payloads containing credentials out of updater logs.
