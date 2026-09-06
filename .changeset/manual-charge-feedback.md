---
"ftw": patch
---

EV modal, Manual tab: after Charge now the line under the button follows the charger instead of repeating the request. It says that the amps were sent and the box is waiting for the charger to confirm, that the charger has taken the limit and the car has not started drawing, that the car is charging, that the charger offers the current but the car is not drawing it (with the charger's own reason, such as "EV not accepting current"), that the command stalled, or that the main fuse limits the charge right now — each with the time elapsed. The plan strip above the tabs says the same while a manual charge runs, so the charger's reason is no longer hidden behind the manual sentence. A refused Start (403, 404, 409) now reads as a failure with the server's reason instead of "Charging at 16 A".

`GET /api/loadpoints` carries this as `manual` per loadpoint: `state` (`sent`, `accepted`, `charging`, `not_drawing`, `stalled`, `limited`), `started_at_ms`, `since_ms`, requested and commanded watts and amps, the charger's reported limit and reason. `POST …/manual_hold` answers with `started_at_ms`, and an Update of the amps keeps the first press as the start. `commanded_since_ms` says when the box's current order was first given.
