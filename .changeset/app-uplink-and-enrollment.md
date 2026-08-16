---
"ftw": minor
---

The box now speaks the FTW app's protocol for real. It mints its own Noise static key, a long-lived rendezvous secret and single-use pairing codes, prints them as the QR payload the app parses, and holds an outbound connection to the content-blind relay under a handle that changes every hour. The rotating handle removes a stable protocol ID; it does not hide the source IP, timing or connection pattern, which the relay can use to correlate a household. Up to four phones share the uplink; each gets its own encrypted session, and the box tells them apart by which one's key authenticates a frame, because the relay cannot say. A phone is let in by a pairing code once and by its own key thereafter, so reconnecting never needs a new code and a photographed code cannot pair a second device.

The app link is on when an existing config omits the `app_link` section, so
upgraded stable boxes join the supported relay without a manual YAML edit.
`app_link.enabled: false` remains an explicit opt-out.

`controlRev` now means something: it is a fingerprint of the site's controllable state, so it moves when the mode or a target does and holds still through the per-tick churn. A command that expected an older revision is refused, and a session that falls behind is resynced with a fresh snapshot rather than left refusing every command until it reconnects. Site mode changes from every door now go through one function, so none can arrive having set the mode without dropping the manual hold and resetting the PI integrator.
