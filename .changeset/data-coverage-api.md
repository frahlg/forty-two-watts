---
"ftw": minor
---

Settings now says when a location is outside a data source's domain, instead of leaving the operator to infer it from an empty chart.

`GET /api/data-sources` reports every forecast and price source with its coverage area, country list, licence, whether a key is needed, and whether it reaches this site. The Weather tab renders that under the map; the Price tab flags a pin that sits outside every European bidding zone. Weather and PV forecasting still work worldwide; price-driven planning remains Europe-only, and there is still no manual or fixed-tariff provider. See `docs/data-coverage.md`.
