# Self-update

In-app "Update" and "Restart" buttons trigger `docker compose pull` +
recreate of the main service. The mechanism is split across three
processes so the main container never touches the Docker socket.

## Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                         host (Pi / server)                          │
│                                                                     │
│  ┌─────────────────────────┐     ┌──────────────────────────────┐   │
│  │  forty-two-watts        │     │  ftw-updater (sidecar)       │   │
│  │  ───────────────        │     │  ─────────────               │   │
│  │  selfupdate.Checker     │     │  net/http on UDS             │   │
│  │  - polls GH Releases    │◀────┤  POST /update {action}       │   │
│  │  - serves /api/version  │ UDS │  GET /status                 │   │
│  │    /* endpoints         │     │                              │   │
│  │  - reads state.json     │     │  shells out:                 │   │
│  │                         │     │   docker compose pull        │   │
│  │                         │     │   docker compose up -d       │   │
│  │                         │     │     [--force-recreate]       │   │
│  └─────────┬───────────────┘     └──────────┬───────────────────┘   │
│            │                                │                       │
│            │    update-ipc (tmpfs volume)   │    /var/run/docker.sock │
│            └────────────────┬───────────────┘    (bind mount)       │
│                             │                                       │
│                      state.json + sock                              │
└─────────────────────────────────────────────────────────────────────┘
```

## Why a sidecar

The main container can't restart itself mid-request — killing its own
process during `docker compose up -d` would drop the HTTP response and
leave the UI polling into the void. A separate container that outlives
the main service's recreate cycle handles this cleanly.

Giving the main container access to `/var/run/docker.sock` would also
grant it root-equivalent access to the host. The sidecar localizes that
privilege to one small binary (~250 LOC, no network, no persistent
storage, a read-only bind of `docker-compose.yml`).

## State transitions

`state.json` is written to the shared `update-ipc` tmpfs volume. Every
step rewrites the whole file atomically (`tmp → rename`). Both ends
treat it as authoritative.

```
idle → pulling → restarting → done
                ↘
                  failed (on error, with stderr tail)
```

The tmpfs volume outlives the recreate of either container, so the new
main container reads `done` on startup and serves it to the UI that's
still polling in the browser — which then hard-reloads.

## The six endpoints

| Endpoint | Purpose |
|---|---|
| `GET  /api/version/check?force=1` | Cached GH Releases probe. `?force=1` bypasses the 3 h cache. |
| `POST /api/version/skip` `{version}` | Persist a dismissed version. Hides the badge until something newer ships. |
| `POST /api/version/unskip` | Clear the skip so the current latest resurfaces. |
| `POST /api/version/update` | Trigger `pull` + `up -d` for the currently-latest tag. |
| `POST /api/version/restart` | Trigger `pull` + `up -d --force-recreate`. Exists so the full flow can be exercised locally without waiting for a real release. |
| `GET  /api/version/update/status` | Pass-through of the sidecar's `state.json`. Polled every 2 s by the UI during the countdown. |

## Testing locally

```bash
# Bring the stack up with the sidecar
docker compose up -d

# Verify both services are running
docker compose ps

# Open the UI, click the version text in the top-right header
# → modal opens → click "Restart (test)"
# → the overlay counts down while the sidecar runs pull + up -d --force-recreate
# → the new main container writes state=done
# → UI hard-reloads into the (same) version
```

If the sidecar isn't running, the Update/Restart buttons return 502 and
the badge still works as a notify-only indicator.

## Skip semantics

`update.skipped_version` (in the `state.db` `config` KV) holds at most
one version string. `Checker.Info` reports `skipped=true` only when the
persisted value equals the currently-latest release. That means a newer
release automatically re-surfaces the banner without asking the user to
un-skip — we never silently hide something the user didn't explicitly
dismiss.

"Check for updates" in the UI (shown when you open the modal while
already on the latest version) also clears the skip before probing, so
a version you hid earlier resurfaces as soon as you ask about it.

## Hardening options (not v1)

- **Restrict socket access**: put `tecnativa/docker-socket-proxy` in
  front of the socket mount and whitelist only `POST /images/create`
  and `POST /containers/*/restart`.
- **Image signature verification**: call `cosign verify` inside the
  sidecar before `up -d`, rejecting images that don't match the
  release-signing key.
- **Rollback**: snapshot the pre-update image digest so a subsequent
  "Rollback" button can retag and recreate.

## Enabling and disabling

The entire feature is gated on `FTW_SELFUPDATE_ENABLED=1`. The shipped
`docker-compose.yml` sets this on the main service; any deploy that
doesn't use the sidecar (bare-metal binary, native OS image, dev build
started with `go run`, etc.) leaves it unset and the UI hides the badge
entirely. Handlers under `/api/version/*` return `503 self-update
disabled` when the flag is off.

Finer-grained knobs, only relevant once the feature is enabled:

- `FTW_UPDATER_SOCKET=""` — keep the GH probe and the "Update available"
  banner but disable the Update/Restart buttons. The UI shows the
  notification dot and release notes; clicking Update surfaces a 502.
- Remove the `ftw-updater` service block from `docker-compose.yml` — the
  main container ignores the missing socket gracefully and behaves the
  same as the previous option.

Future native / OS-image builds will ship their own update mechanism and
either leave this flag off (and wire their own gate) or reuse the same
name with a different backend.
