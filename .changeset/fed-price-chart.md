---
"ftw": patch
---

The price chart can now be handed its data instead of fetching it. A `fed` attribute turns off both the request to /api/prices and the five-minute poll behind it, and `setPrices()` pushes a window in. The FTW app draws this same chart over its encrypted session to the box and has no HTTP origin to fetch from, so without this it would have to keep a fork — and a forked chart is one that stops matching the box's own.

A slot fed with its own total is shown as sent rather than recomputed from the box's tariff and VAT, since the caller may hold different ones, and the subtitle says "total to import" rather than naming parts it cannot see. The dashboard is unchanged: without the attribute the component fetches, polls and labels exactly as before.
