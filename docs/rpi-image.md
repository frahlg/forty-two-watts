# Raspberry Pi SD-card image

A pre-built `42w-rpi4-arm64-vX.Y.Z.img.xz` ships with every tagged
release. Flash it to an SD card, drop it in a Raspberry Pi 4, and the
dashboard comes up at `http://42w.local:8080/` with zero terminal
work on the user's side.

The image is plain Raspberry Pi OS Lite (64-bit, Bookworm) with:

- **Docker + compose plugin** — installed from the official Docker
  apt repo, same version stream as the `scripts/install.sh` bare-metal
  installer.
- **`docker-compose.yml` baked in** — a copy of the compose file at
  the release tag, placed at `/home/ftw/forty-two-watts/`. The stack
  is not started at image-build time; it's pulled + started on first
  boot against GHCR.
- **mDNS via avahi** — `TARGET_HOSTNAME=42w` makes the Pi reachable
  at `42w.local` on any LAN that respects mDNS (essentially all of
  them).
- **`ftw-firstboot.service`** — a systemd oneshot that runs
  `docker compose pull && up -d` with a 6-attempt retry loop, then
  touches `/var/lib/ftw/firstboot.done` so subsequent boots are no-ops.
- **WiFi captive portal** — `42w-wifi-onboarding.service` brings up a
  `42w-setup` access point + captive portal if NetworkManager doesn't
  have a live connection ~30 s after boot. Backed by balena-os's
  [`wifi-connect`](https://github.com/balena-os/wifi-connect).

## Flash it

1. Download the latest `42w-rpi4-arm64-vX.Y.Z.img.xz` from
   [Releases](https://github.com/frahlg/forty-two-watts/releases).
2. Flash with [Raspberry Pi Imager](https://www.raspberrypi.com/software/)
   — **Choose OS** → **Use custom** → pick the `.img.xz`. Imager handles
   xz decompression + writing.
3. Before writing, open Imager's advanced-options panel (gear icon) to
   pre-configure hostname (default `42w` is fine), SSH key, WiFi SSID +
   password, and timezone. Anything you set here overrides the image
   defaults.
4. Insert the card, power on the Pi. First boot pulls the docker
   images (~60–90 s on a decent connection) and brings up the stack.

## WiFi onboarding

You have two paths. Pick whichever the flashing user can actually
execute.

### Path 1 — Raspberry Pi Imager (preferred)

Pre-configure WiFi credentials in Imager's advanced options before
flashing. The Pi connects to the named network at first boot and the
captive-portal fallback never triggers.

### Path 2 — captive portal

If WiFi wasn't pre-configured, the Pi exposes a `42w-setup` access
point about 30 seconds after boot. From a phone or laptop:

1. Connect to `42w-setup` (no password).
2. Your device should auto-open the captive portal. If not, visit
   `http://192.168.42.1/` in a browser.
3. Pick your home network from the list, enter the password, submit.
4. The Pi joins your network, the AP disappears, and the dashboard
   comes up at `http://42w.local:8080/` within 30–60 s.

Captive-portal detection is reliable on Android and older iOS. iOS 17+
occasionally requires you to manually open a browser and visit any
HTTP (not HTTPS) URL — the portal will intercept it.

## Open the dashboard

```
http://42w.local:8080/
```

First-time users land in the setup wizard at `/setup`. If `42w.local`
doesn't resolve, find the Pi's IP on your router's client list and
use that instead.

## Troubleshooting

**The Pi boots but `42w.local` doesn't respond.**
SSH in (`ssh ftw@42w.local`, default password `fortytwowatts` unless
you overrode it in Imager) and check:

```bash
systemctl status ftw-firstboot     # first-boot provisioner
journalctl -u ftw-firstboot -b     # its log (since this boot)
tail -f /var/log/ftw-firstboot.log # its durable log
docker compose -f /home/ftw/forty-two-watts/docker-compose.yml ps
```

If `ftw-firstboot` failed (bad network, GHCR outage), the service is
idempotent — `systemctl restart ftw-firstboot` re-runs it.

**I want to re-onboard WiFi from scratch.**

```bash
sudo rm /var/lib/ftw/wifi-configured
sudo nmcli connection delete "<your old SSID>"
sudo reboot
```

Next boot, the captive portal comes up again.

**The dashboard is up but I want to reinstall from zero.**

```bash
cd /home/ftw/forty-two-watts
sudo docker compose down -v         # drops state — PV model, battery
                                    # model, price/load history
sudo rm -rf data mosquitto/data
sudo rm /var/lib/ftw/firstboot.done
sudo reboot
```

## Building the image yourself

All the image provisioning lives in `deploy/pi-gen/`. Any Linux host
with Docker (or macOS with Docker Desktop) can build it:

```bash
deploy/pi-gen/build.sh
```

The script clones [pi-gen](https://github.com/RPi-Distro/pi-gen) into
`deploy/pi-gen/pi-gen/`, symlinks our `config` + `stage-42w/` in, and
runs pi-gen's dockerised build. Output lands at
`deploy/pi-gen/pi-gen/deploy/42w-rpi4-arm64-*.img.xz`. A full build
takes ~30–45 minutes on a decent laptop and ~15 GB of working disk.

CI runs the same script from
[`.github/workflows/release.yml`](../.github/workflows/release.yml) —
see the `rpi-image` job — and uploads the result to the GitHub
release that triggered it.
