---
"ftw": patch
---

The price chart's tooltip now reads the day it is drawing. The chart shows the horizon its toggle selects — today, tomorrow, or both — but the tooltip looked its slot up in every slot the box had sent, so with **Tomorrow** showing, the bar under your finger was tomorrow's and the price beside it was today's. Both days start at midnight, so the clock time printed correctly over the wrong day's number: on a phone, a bar at the top of a 96 öre axis read "9.00 öre". The slots being drawn now travel with the geometry that hit-tests them, since those are the two things that have to agree.
