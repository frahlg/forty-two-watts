#!/bin/bash -e
# Runs OUTSIDE the chroot. File manipulation uses ${ROOTFS_DIR}/...
# paths; chroot operations go through on_chroot. pi-gen's own
# stages follow this same pattern (see stage2/01-sys-tweaks) —
# *-run-chroot.sh scripts are piped to on_chroot as stdin and
# therefore can't read the files/ directory, hence the split.

# Docker — explicit apt repo + minimal package set. Replaces
# `curl get.docker.com | sh` which pulls docker-ce-rootless-extras +
# docker-buildx-plugin + docker-model-plugin (~300 MB combined). We
# don't run rootless, don't build images on the Pi, and have no AI
# workloads — so those stay out. The bare four packages below are
# what `docker compose up -d` against a pulled image actually needs.
# Same Docker apt repo URL and pinning approach the convenience
# script uses, so we stay on the same engine version stream.
on_chroot << 'EOF'
set -e
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc
echo "deb [arch=arm64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian bookworm stable" \
    > /etc/apt/sources.list.d/docker.list
apt-get -qq update
DEBIAN_FRONTEND=noninteractive apt-get -y -qq install --no-install-recommends \
    docker-ce docker-ce-cli containerd.io docker-compose-plugin
systemctl enable docker.service
systemctl enable avahi-daemon.service
systemctl enable NetworkManager.service
# /etc/hosts entry prevents sudo's "unable to resolve host 42w"
# warning on first boot. pi-gen writes /etc/hostname from
# TARGET_HOSTNAME but leaves /etc/hosts at the stock Raspberry Pi
# OS template (which hard-codes `raspberrypi`).
sed -i 's/^127\.0\.1\.1.*/127.0.1.1\t42w/' /etc/hosts
EOF

# init=firstboot on first boot — without stage2, nobody else injects
# this into cmdline.txt. The firstboot script ships with
# raspberrypi-sys-mods and handles partition resize, ssh enable, and
# userconf-pi processing on the very first boot, then removes itself
# from cmdline.txt and reboots. Mirrors what pi-gen's
# stage2/01-sys-tweaks/01-run.sh does on a stock Lite build.
# Idempotent — re-running the build won't double-inject.
CMDLINE="${ROOTFS_DIR}/boot/firmware/cmdline.txt"
if [ -f "${CMDLINE}" ] && ! grep -q "raspberrypi-sys-mods/firstboot" "${CMDLINE}"; then
    sed -i 's| rootwait| init=/usr/lib/raspberrypi-sys-mods/firstboot rootwait|' "${CMDLINE}"
fi

# Deploy directory: docker-compose.yml lives here and
# `docker compose up -d` runs from it on first boot.
#
# We write into /home/pi/ at build time, NOT /home/ftw/, even though
# FIRST_USER_NAME=ftw and the resulting account is `ftw`. Why:
# pi-gen's bookworm-arm64 branch defers the `pi → ftw` rename to
# FIRST BOOT (export-image/01-user-rename schedules a wizard). At
# first boot, `usermod -m` moves /home/pi/* into /home/ftw/ —
# and any pre-existing /home/ftw/ contents get clobbered in the
# process. Writing to /home/pi/ means our files travel with the
# rename and end up at /home/ftw/forty-two-watts/ exactly as
# firstboot.sh expects. Confirmed by run 24881931326 producing an
# image where /home/ftw/ was empty after first boot.
#
# data/ is chowned 100:101 because the in-container ftw user
# (alpine `adduser -S`) needs to own it before SQLite can create
# state.db. Same UID/GID mapping as scripts/install.sh.
install -d -m 0755                    "${ROOTFS_DIR}/home/pi/forty-two-watts"
install -d -m 0755 -o 100 -g 101      "${ROOTFS_DIR}/home/pi/forty-two-watts/data"
install -d -m 0755                    "${ROOTFS_DIR}/home/pi/forty-two-watts/mosquitto"
install -d -m 0755                    "${ROOTFS_DIR}/home/pi/forty-two-watts/mosquitto/config"

install -m 0644 files/docker-compose.yml    "${ROOTFS_DIR}/home/pi/forty-two-watts/docker-compose.yml"
install -m 0644 files/mosquitto.conf        "${ROOTFS_DIR}/home/pi/forty-two-watts/mosquitto/config/mosquitto.conf"

install -m 0755 files/firstboot.sh          "${ROOTFS_DIR}/usr/local/sbin/ftw-firstboot"
install -m 0644 files/firstboot.service     "${ROOTFS_DIR}/etc/systemd/system/ftw-firstboot.service"

on_chroot << 'EOF'
systemctl enable ftw-firstboot.service
EOF
