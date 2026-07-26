---
"ftw": minor
---

Add the four Blixt L1 host services FTW was still missing, so a driver converted in `srcfl/device-drivers` runs here unchanged. `host.set_model(name)` and `host.set_rated_w(watts)` record the rest of the nameplate beside make and serial, and the host repeats both on every emit so they reach Nova's `model` and `rated_power_w` without the driver restating them each poll. `host.set_warmup_s(seconds)` holds off the first poll for a device that answers Modbus before its registers are meaningful. `host.decode_string(registers, start, count)` reads ASCII from a register block — two characters per register, high byte first, trailing padding stripped — replacing the byte loop a dozen catalog drivers hand-roll. Nothing is removed and no existing driver behaves differently.
