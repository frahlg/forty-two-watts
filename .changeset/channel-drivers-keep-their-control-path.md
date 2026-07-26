---
"ftw": minor
---

A driver downloaded from the signed channel can now control the hardware it was ported to control. FTW refused any device-drivers artifact that was not marked read-only, and the channel marked all of them read-only, so the same source file could drive a battery when it shipped with the build and could not when it was downloaded. A driver published with a control path now runs under the same terms as the bundled copy of that same source. A driver that declares itself read-only — a meter, a telemetry gateway — still gets a read-only policy, and a manifest whose read_only and control_enabled disagree is refused outright.
