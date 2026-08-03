---
"ftw": patch
---

A Modbus device that answers but refuses a register no longer costs the whole
poll. Only a failure to reach the device does.

The rule was `attempts == successes`: one register missing out of twenty threw
away every reading in that poll, including the ones that arrived. That made a
driver's own tolerance count for nothing — `sungrow.lua` marks 19 of its 20
reads optional precisely so a partial read still reports — and it made the
driver permanently useless on a string inverter, which has no battery
registers and refuses them on every poll for as long as it is installed.

A device replying "illegal data address" is stronger proof of life than a
register that read cleanly: it replied. So a poll is now current when
something was read and nothing failed at the transport. Reads skipped because
a reconnect backoff was already running are counted separately too — they are
downstream of one transport failure, not fresh evidence of several, which is
why a single dropped packet used to report as "8 of 20 modbus reads failed"
when seven of those eight never reached the wire.

The guarantee that matters is unchanged: an unreachable device still fails
every poll, so a stale site meter still stops dispatch. Poll errors now say
which of the two happened.
