---
"ftw": patch
---

A passkey prompt no longer fires on every configure call. The box refused each configure-tier write until the app carried a fresh step-up, and it remembered nothing about the ceremony that had just run, so a settings screen that subscribes, saves its rules and sends a test cost three Face IDs — and another for every failed attempt.

A genuine ceremony now opens a short grace window on that session: five minutes, measured on the box's uptime clock, the same monotonic clock a command's expiry is checked against, so a wall-clock jump cannot widen it. Inside the window, further configure calls from the same session are accepted without a fresh ceremony; outside it, the next write pays one again. The window runs only from a real ceremony and is never extended by a call it waved through, so it outlives a ceremony by at most five minutes and no run of writes holds it open unattended. It is bound to the one enrolled device the session serves, so no phone inherits another's. The property is unchanged — a privileged action still needs recent human presence — and only the re-prompting inside that window is gone.
