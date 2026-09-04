---
"ftw": patch
---

The Devices card no longer has its own pencil editor for the car's charge level; the slider in the EV card owns that value (#1062). The card still shows the charge, now as one number with its source in plain words — "estimated", "from the car", or "pinned after the car stopped asking" — and, while a car is plugged in, a line saying where to change it. Two leftovers from an older EV power slider were removed from the dashboard script; they had no control on the page and nothing called them. The HTTP routes behind them are unchanged.
