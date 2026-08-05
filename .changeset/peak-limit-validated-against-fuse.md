---
"ftw": patch
---

The peak-shaving import limit is now checked against the site's fuse. It
used to be stored exactly as sent, from the API and from the Home
Assistant number alike, so a limit above the breaker was accepted and
then never bound: every import clamp already stops at the fuse, so the
operator read their number back from the status page and believed a
tariff peak was defended that nothing was defending. A negative limit was
worse than useless — the shaving arm treats it as an error to correct and
commands the battery to push power out, from a setting named for import.
Both are refused now, with a message naming the value sent and the
ceiling that beat it. A limit of 0 still means what it always meant in
peak shaving, "correct everything above zero import"; peak shaving is
switched off by leaving the mode, not by zeroing its threshold. A site
whose fuse is not described in the config keeps the old behaviour. When a
config reload lowers the fuse under a limit that was legal when it was
set, the log says so.
