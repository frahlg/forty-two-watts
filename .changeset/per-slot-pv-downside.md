---
"ftw": minor
---

The planner's PV-forecast hedge is now proportional per slot instead of
one flat watt figure across the whole horizon: the PV model learns the
relative forecast error online, and each slot's downside is that share
of its own expected generation — large on variable cloudy days, zero at
night, no longer erasing morning and evening shoulders or hedging a
clear tomorrow with today's uncertainty. Measured against real
snapshots the flat haircut cost 25–65 SEK per 48 h plan. Sites where
the model has not yet learned the relative error keep the previous
flat behavior.
