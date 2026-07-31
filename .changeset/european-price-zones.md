---
"ftw": minor
---

Prices: pick any of the 46 European bidding zones the price API publishes,
and be billed in the currency of the country you picked.

The zone picker offered twelve Nordic codes typed into two `<select>`
elements and the Go side knew twelve EIC codes, so a household in Belgium,
the Netherlands or Spain could not choose its own zone even though the
Sourceful API has served every ENTSO-E area all along. Both lists now come
from one table, `go/internal/prices/zones.go`, generated from that API's
`/areas` endpoint and served to the UI at `GET /api/prices/zones`. The
picker asks for a country first and a zone second, because everyone knows
they live in Italy and nobody knows their area code is `IT-CENTRE-NORTH`.

Currency stops being Swedish by assumption. It defaults to the currency of
the chosen zone, and the price API — which converts to EUR and SEK and
quietly answers anything else with EUR — is only asked for those two;
every other currency is converted here from EUR with the ECB rates the
service already caches. Where no rate is available the fetch fails rather
than storing a number that is wrong by an exchange rate, which is a number
the planner would spend money on. The old 11.5 SEK/EUR fallback is gone for
the same reason.

Two related faults fixed on the way. The direct ENTSO-E provider assumed
every day-ahead document is priced in EUR; Poland and Hungary among others
publish in their own currency, so it now reads `currency_Unit.name` and
converts from that. Its EIC code for Germany was the country code rather
than the DE-LU bidding zone, and NO5 was missing outright.

Prices are stored as minor units per kWh with no currency attached, so
changing the currency clears the price cache — otherwise cost history would
add öre to cent. The next fetch refills today and tomorrow. Every price
label in the UI now follows the configured currency: öre, cent, øre, grosz,
Rappen, or the major unit where the minor one is out of circulation
(4.30 Kč/kWh, not 430 haléř).

Existing installs are untouched: no zone means SE3, no currency means SEK,
and a Swedish install still asks the API for SEK exactly as before.
