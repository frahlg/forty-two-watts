---
"ftw": patch
---

The FTW app can draw electricity prices. The box answers `price.get` over the app protocol with the slots covering a window, spot and total as whole minor units per kWh — rounded once, here, so the app and the box's own dashboard cannot end up disagreeing about what 18.7 öre is. `price.spot` is advertised only when a zone is configured and rows are stored, so a house with no price feed gets no empty chart instead of a broken one.

An answer that does not cover the window asked for says so, at either edge or in the middle. Tomorrow's rates publish in the afternoon, a failed midday fetch leaves a hole, and a box that first heard from the market at breakfast holds nothing for the hours before it; all three come back marked stale rather than as a market that went quiet. A window is read eight days at a time — the price table is never pruned and the app derives its window from the phone's clock, so a wrong clock used to mean a query over every row the box had ever stored.
