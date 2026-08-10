---
"ftw": patch
---

New notification kinds reach boxes that chose their rules before the kinds existed. The rules document served a stored configuration verbatim, so the first household to ever save a rule was also the last that could ever see a kind added later — the charging and update notifications were invisible on any box with a pre-existing notifications section. The effective document now appends any known type the stored list lacks, at its default and disabled; the stored entries themselves are never touched and nothing is written by reading.
