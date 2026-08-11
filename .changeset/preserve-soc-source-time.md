---
"ftw": patch
---

Preserve the last fresh SoC time across power-only and explicitly cached vehicle updates. Do not duplicate long-format SoC samples for those updates. Vehicle control now ages SoC from that observation time and rejects driver-marked stale data.
