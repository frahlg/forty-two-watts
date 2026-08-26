---
"ftw": patch
---

A short Modbus network blip no longer leaves a battery or meter offline until the box is restarted. Drivers that skip a register after a few failed reads used to skip every register after a "no route to host" moment; the host now reloads that driver and resumes polling once the link is back.
