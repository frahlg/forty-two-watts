---
"ftw": patch
---

Load model: the outlier filter could lock the model out of learning a load
level it had not seen before, permanently. A rejected sample updates
neither `MAE` nor `Samples`, so the band `max(MAE × 10, 200)` never grew in
response to being persistently wrong; combined with the filter arming after
50 samples — 51 minutes at the 60 s cadence — a model calibrated on one
quiet hour rejected the real house forever.

Measured on a clean model: an hour at 400 W left MAE at 57 W and the band
at 570 W. A following week at 5 kW was rejected in full, 100% of samples,
and the prediction never moved off 1794 W. The failure is silent and looks
like a bad forecast from the outside, because the model reports a small
error while being arbitrarily wrong.

Three changes:

- A hard bound at `3 × PeakW`, always on. A sample above the site's rated
  draw is a measurement fault, not a household. It is absolute — derived
  from configured hardware, never from what the model has learned — so it
  holds from the first sample and cannot be widened by a model that has
  mislearned.
- The soft MAE filter now arms after a day of samples rather than an hour,
  so its band reflects a full night-and-day cycle instead of whichever
  arbitrary hour followed the last restart.
- Ten consecutive same-direction rejections widen the band by exactly
  enough to admit the residual, after which the ordinary EMA takes over.
  Spikes are short and alternate in sign; a real level shift is sustained.

A three-minute 6 kW spike is still rejected in full. A sustained shift to a
new level is picked up within ten minutes and tracked exactly.
