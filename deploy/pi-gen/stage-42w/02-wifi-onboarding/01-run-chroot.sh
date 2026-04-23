#!/bin/bash -e
# Install balena-os/wifi-connect so first-time users with no pre-configured
# WiFi get a captive portal instead of a silent brick. The service runs
# once on boot; if NetworkManager already has a live connection it's a
# no-op, otherwise it brings up a `42w-setup` AP with the balena UI.

WIFI_CONNECT_VERSION="${WIFI_CONNECT_VERSION:-v4.11.84}"
WIFI_CONNECT_BIN_URL="https://github.com/balena-os/wifi-connect/releases/download/${WIFI_CONNECT_VERSION}/wifi-connect-aarch64-unknown-linux-gnu.tar.gz"
WIFI_CONNECT_UI_URL="https://github.com/balena-os/wifi-connect/releases/download/${WIFI_CONNECT_VERSION}/wifi-connect-ui.tar.gz"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

curl -fsSL -o "${TMPDIR}/wc.tar.gz"    "${WIFI_CONNECT_BIN_URL}"
curl -fsSL -o "${TMPDIR}/wc-ui.tar.gz" "${WIFI_CONNECT_UI_URL}"

tar -xzf "${TMPDIR}/wc.tar.gz" -C "${TMPDIR}"
install -m 0755 "${TMPDIR}/wifi-connect" /usr/local/sbin/wifi-connect

install -d -m 0755 /usr/share/wifi-connect/ui
tar -xzf "${TMPDIR}/wc-ui.tar.gz" -C /usr/share/wifi-connect/ui

install -m 0755 files/42w-wifi-onboarding            /usr/local/sbin/42w-wifi-onboarding
install -m 0644 files/42w-wifi-onboarding.service    /etc/systemd/system/42w-wifi-onboarding.service

systemctl enable 42w-wifi-onboarding.service

# Bookworm's default dhcpcd setup fights NetworkManager over the wlan
# interface. Raspberry Pi OS Lite has shipped with NM as the default
# network stack since 2023, but the dhcpcd service is still installed
# and running on a stock stage2 image. Disable it to keep NM in sole
# control — otherwise wifi-connect's AP mode toggling races the dhcpcd
# wlan0 supplicant and both sides lose.
systemctl disable dhcpcd.service 2>/dev/null || true
systemctl mask    dhcpcd.service 2>/dev/null || true
