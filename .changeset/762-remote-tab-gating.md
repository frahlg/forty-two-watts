---
"ftw": patch
---

Settings → Remote no longer offers passkey setup it cannot start. The section's gating toggled a `hidden` class no stylesheet defines, so the setup controls stayed visible and clickable while the gateway identity wasn't adopted — clicking opened a blank tab that immediately closed itself (a white flicker), and the error text was overwritten by the next status poll. The gating now uses the DOM hidden property, the click refuses with a durable explanation before any tab opens, and enrollment failures land in an error element the poller doesn't rewrite.
