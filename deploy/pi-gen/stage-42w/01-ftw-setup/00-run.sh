#!/bin/bash -e
# Runs OUTSIDE the chroot. File manipulation uses ${ROOTFS_DIR}/...
# paths; chroot operations go through on_chroot. pi-gen's own
# stages follow this same pattern (see stage2/01-sys-tweaks) —
# *-run-chroot.sh scripts are piped to on_chroot as stdin and
# therefore can't read the files/ directory, hence the split.

# Docker via the official convenience script — same path as
# scripts/install.sh so bare-metal and image installs converge on
# the same engine version. Runs inside the chroot so the installed
# service units end up in the image rootfs.
on_chroot << 'EOF'
curl -fsSL https://get.docker.com | sh
systemctl enable docker.service
systemctl enable avahi-daemon.service
# /etc/hosts entry prevents sudo's "unable to resolve host 42w"
# warning on first boot. pi-gen writes /etc/hostname from
# TARGET_HOSTNAME but leaves /etc/hosts at the stock Raspberry Pi
# OS template (which hard-codes `raspberrypi`).
sed -i 's/^127\.0\.1\.1.*/127.0.1.1\t42w/' /etc/hosts
EOF

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
