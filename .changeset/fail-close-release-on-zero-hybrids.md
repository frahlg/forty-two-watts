---
"ftw": patch
---

Make the bundled Huawei and Ferroamp Modbus hybrid drivers telemetry-only.
Their former 0 W battery commands released the inverter to its own control
instead of holding the battery idle. Ferroamp battery control remains available
through its hardware-tested MQTT driver.
