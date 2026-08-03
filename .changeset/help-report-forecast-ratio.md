---
"ftw": patch
---

Help report: the forecast check now compares forecast and reality as a ratio,
and the load-model note no longer repeats an explanation that turned out to be
wrong.

A live report from a v1.15.0 install showed a plan slot sized for 1.47 kW
against a house drawing 3.65 kW — two and a half times out — and Findings said
nothing. The old measure divided the difference by the larger figure, which
saturates at 1.0 and therefore cannot express "twice wrong" at all: that case
scored 0.597 against a 0.6 threshold. A solar forecast of 11.7 kW against
7.4 kW actual scored 0.584 and was missed the same way. Both are exactly what
the check exists to catch. It is now `max/min ≥ 1.5`, which has no ceiling, on
figures above a 500 W floor.

The load-model paragraph claimed that discarding negative samples makes a
large-solar site's model skew low. Discarding the lower tail biases the
surviving mean *high*, so the explanation was backwards. It now describes what
the model actually does — one sample a minute into 168 hourly buckets — and
what a real problem looks like: a small average error next to a gap that
persists across several plan slots.

A forecast gap on an install whose model has not finished learning is now a
note rather than a problem, and says so, instead of contradicting the "still
learning" line two rows below it and suggesting a reset that would only
restart the clock.
