---
"ftw": patch
---

A household can get back to no phones paired, from the box's own page. Until now the last owner could not be removed at all: the device list drew a sentence where the Remove button goes — add another before removing this one — so a home that lost its only phone, or wanted to start clean, had no way down to an empty list from anywhere.

The refusal was right about one door and wrong about the other. What it guards against is a phone emptying the roster from anywhere in the world: remove the last owner over a session and nobody can administer the box, and nothing done remotely can mend that. Standing at the box is a different position. Whoever reads that page is in the building, and the same page mints a fresh pairing code — the way back in is the button above the list. Refusing there protected nothing and stranded the household the rule was written for.

So `appenroll.Revoke` now takes the presence of whoever is asking, and refuses the last owner over a session only. The decision stays in enrollment rather than moving up to a screen; what is new is that the box is told which door the request came through. That fact comes from the caller the box named when it admitted the request — a LAN caller is minted as a local owner, an app session carries the kind `app` — and never from a field a remote caller could set. Removing the last owner through the app is still `E_LAST_OWNER_PROTECTED`, the same sentence and the same code.

Stepping the last owner down is still refused at both doors, the box's own page included. Removing the last phone leaves a list somebody at the box can fill again; demoting it leaves a list of phones that can only look, which is the lockout rather than a way out of it.

The box's device list follows. The last owner's row carries the Remove button every other row has, and the warning lands where it belongs — at the press, saying what is on the other side of it. A household down to one phone is told that nothing will see or change this home until a phone is paired again, with a code from this page. One with guests still paired is told those phones will be left able to look and change nothing. Both are what happens, which the old sentence was not: it described a rule the box no longer keeps.
