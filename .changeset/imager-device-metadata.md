---
"ftw": patch
---

Raspberry Pi Imager: the repository manifest now declares the hardware it
supports, so the documented install flow reaches the FTW image instead of
dead-ending on an empty device chooser.

Imager 2.x opens on "Select your Raspberry Pi device" and builds that list
solely from `imager.devices` in whichever manifest it was pointed at — a
custom repository replaces the stock manifest rather than extending it. FTW
published neither an `imager` object nor a `devices` array, so `HWListModel`
bailed with "missing imager", the device list stayed empty, and **Next**
stayed disabled: the OS list had loaded, so the offline escape hatch
(`osListUnavailable && hwlist.count === 0`) did not apply. Nothing on the
device step could be clicked and the FTW entry was never reached.

The entry itself was also untagged. Imager keeps a `devices`-less OS entry
only when the selected device matches inclusively, so a Pi 5 — which matches
exclusively — would have filtered FTW out even once a device was selectable.
`devices` is required by Imager's published schema for a direct-image entry.

The manifest now ships `imager.devices` (Pi 5, Pi 4, and "No filtering") and
tags the FTW entry `pi5-64bit` / `pi4-64bit`, matching the arm64 image it
actually builds. Both workflows that publish the manifest assert the two
lists are present and agree, so an entry tagged for a device the chooser
never offers fails CI rather than shipping unreachable.
