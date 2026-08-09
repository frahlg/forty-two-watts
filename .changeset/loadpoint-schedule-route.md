---
"ftw": minor
---

An EV charging schedule can now be saved from the app. The schedule gets its own route — `PUT /api/loadpoints/{id}/schedule` stores one, `DELETE` clears it — priced Configure where its sibling target route stays Actuate. The split is the point: a schedule is a standing instruction about future days, so one that arrives late is the same instruction, only later, which is what Configure means. The one-shot fields on the target route — target, soc, force_start — move energy now, so they stay on Actuate and the passthrough keeps refusing them. The on-box page keeps saving through the target route unchanged; both routes share one apply path.

A schedule also learns which days it covers. `days` is a 7-bit mask, bit 0 Monday through bit 6 Sunday; omitted or zero means every day, so every stored schedule and every old client keeps its behaviour. Without it, "full by 07:00" on a work commute also charged for Saturday. The weekday is the household's, read in the box's own time zone: a deadline at 00:30 Saturday in Stockholm is still Friday in UTC, and skipping Saturday has to mean the household's Saturday. The stored time-of-day stays UTC minutes, so a DST change shifts the local deadline by an hour until the schedule is saved again; storing local minutes instead needs a migration and a UI save-path change, and is deferred on purpose.
