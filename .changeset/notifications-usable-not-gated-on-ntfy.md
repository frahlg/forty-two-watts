---
"ftw": patch
---

A box can turn notifications on again when its ntfy is only half-filled. Web push is engine-owned now — keyed by the subscriptions a phone stores, not by the `provider` field — so "notifications on, no provider configured" is a real, working state. But a box carrying the old default (provider `ntfy`, server `https://ntfy.sh`, no topic — the shape most boxes have) was refused at the door: enabling notifications answered `notifications.ntfy.topic required`, and web push could never reach the phone.

The one ntfy setting nothing works without is the topic; it has no default, and nothing publishes without it. So ntfy counts as active only once a topic is set. Below that it is inactive, not an error: the box enables notifications and delivers over web push, and `NewProvider` installs no ntfy transport that would fail every send — it logs the inactive ntfy once, so the drop is warned rather than silent. A topic with no server to carry it is still a mistake, and still refused. A box that genuinely set both keeps sending over ntfy exactly as before.
