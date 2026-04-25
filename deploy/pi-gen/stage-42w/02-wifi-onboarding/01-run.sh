#!/bin/bash -e
# Runs OUTSIDE the chroot. wifi-connect's binary + UI tarballs are
# downloaded on the host and installed into the chroot via
# ${ROOTFS_DIR}/...; service enable + NetworkManager handoff happens
# inside on_chroot blocks.

WIFI_CONNECT_VERSION="${WIFI_CONNECT_VERSION:-v4.11.84}"
WIFI_CONNECT_BIN_URL="https://github.com/balena-os/wifi-connect/releases/download/${WIFI_CONNECT_VERSION}/wifi-connect-aarch64-unknown-linux-gnu.tar.gz"
WIFI_CONNECT_UI_URL="https://github.com/balena-os/wifi-connect/releases/download/${WIFI_CONNECT_VERSION}/wifi-connect-ui.tar.gz"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

curl -fsSL -o "${TMPDIR}/wc.tar.gz"    "${WIFI_CONNECT_BIN_URL}"
curl -fsSL -o "${TMPDIR}/wc-ui.tar.gz" "${WIFI_CONNECT_UI_URL}"

tar -xzf "${TMPDIR}/wc.tar.gz" -C "${TMPDIR}"
install -m 0755 "${TMPDIR}/wifi-connect" "${ROOTFS_DIR}/usr/local/sbin/wifi-connect"

install -d -m 0755                        "${ROOTFS_DIR}/usr/share/wifi-connect/ui"
tar -xzf "${TMPDIR}/wc-ui.tar.gz" -C "${ROOTFS_DIR}/usr/share/wifi-connect/ui"

install -m 0755 files/42w-wifi-onboarding            "${ROOTFS_DIR}/usr/local/sbin/42w-wifi-onboarding"
install -m 0644 files/42w-wifi-onboarding.service    "${ROOTFS_DIR}/etc/systemd/system/42w-wifi-onboarding.service"

on_chroot << 'EOF'
systemctl enable 42w-wifi-onboarding.service

# Bookworm's default dhcpcd setup fights NetworkManager over the wlan
# interface. Raspberry Pi OS Lite has shipped with NM as the default
# network stack since 2023, but dhcpcd is still installed and running
# on a stock stage2 image. Disable it so NM owns wlan0 uncontested —
# otherwise wifi-connect's AP-mode toggling races the dhcpcd wlan0
# supplicant and both sides lose.
systemctl disable dhcpcd.service 2>/dev/null || true
systemctl mask    dhcpcd.service 2>/dev/null || true
EOF
