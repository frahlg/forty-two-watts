---
"ftw": minor
---

Drivers can declare the commands an operator may send, and Core reads them.
A `controls` list in the `DRIVER` block names the command, gives it a label,
describes its single input — type, bounds, step, unit — and states what counts
as proof the device took it. `/api/drivers/{name}` and `/api/drivers/catalog`
surface it.

This is the description, not the path: nothing sends these commands yet. It
exists because there is currently no way to describe one at all. Settings →
Devices renders a hand-written branch per driver family, so a driver with a
real control surface — the Heishamon heat pump's curve offset, verified
against hardware since June — has no way to reach a person, and its author is
told to write a Home Assistant automation instead (srcfl/ftw#520).

Signed packages already carry this shape in `RuntimeCommand`, but only for
drivers shipping through the signed channel. Bundled and local drivers have no
policy, so the `DRIVER` block is where their declaration has to live.

A declaration the UI could not render is dropped rather than surfaced half
formed: no id, an input type outside number/boolean/string, or a number
missing either bound. A slider with one end is a guess, not a control. Drivers
that declare nothing are unchanged, and the JSON key stays absent for them, so
a client can tell "reports only" from "declares an empty list".
