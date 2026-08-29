---
"ftw": minor
---

An adopted OCPP charger is now a device like any other. It gets a row in
`/api/devices` and under Settings → Devices, keyed on the vendor and serial
from its `BootNotification` rather than on the name it dialled with — that
name is one an installer typed and the charger's own web page can change, so
persistent state keyed on it would not survive a re-commissioning. Rename a
charger and the row follows it. A charger that reports no serial falls back to
the dialled name, recorded as an endpoint so it reads as stable-until-changed.
Pending chargers get no row: a device row says this hardware is part of the
site, and quarantine says an unadopted charge point is not.

`GET /api/ocpp/chargers` now also reports each charger's `serial` and
`firmware`, and OCPP 1.6's deprecated `chargeBoxSerialNumber` is read when the
current field is empty — shipped firmware disagrees about which to fill, and
losing it loses the only stable identity some chargers ever report.

The OCPP server's own settings — on/off, bind address, both ports, path,
username and password — are editable under Settings → Chargers instead of
only in `config.yaml`. TLS paths and per-charger credentials stay in the file:
they are host filesystem paths and one secret per charger, set once at
commissioning.
