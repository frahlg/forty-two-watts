---
"ftw": patch
---

Map driver emit keys onto the names the Nova payload reads. Catalog drivers emit `import_wh`, `export_wh`, `charge_wh`, `discharge_wh`, `lifetime_wh` and `hz`, while `DerTelemetry` reads `total_import_wh`, `total_export_wh`, `total_charge_wh`, `total_discharge_wh`, `total_generation_wh` and `freq_hz` — so those values never left the gateway. The canonical `@srcful/data-models` spellings map onto the same names, so the driver catalog can convert without a further host change.
