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

# /home/ftw is the deploy directory — docker-compose.yml lives here
# and `docker compose up -d` is run from it on first boot. The data/
# subdir is chowned to 100:101 because the in-container ftw user
# (alpine `adduser -S`) needs to own it before SQLite can create
# state.db. Same UID/GID mapping as scripts/install.sh.
install -d -m 0755                    "${ROOTFS_DIR}/home/ftw"
install -d -m 0755                    "${ROOTFS_DIR}/home/ftw/forty-two-watts"
install -d -m 0755 -o 100 -g 101      "${ROOTFS_DIR}/home/ftw/forty-two-watts/data"
install -d -m 0755                    "${ROOTFS_DIR}/home/ftw/forty-two-watts/mosquitto"
install -d -m 0755                    "${ROOTFS_DIR}/home/ftw/forty-two-watts/mosquitto/config"

install -m 0644 files/docker-compose.yml    "${ROOTFS_DIR}/home/ftw/forty-two-watts/docker-compose.yml"
install -m 0644 files/mosquitto.conf        "${ROOTFS_DIR}/home/ftw/forty-two-watts/mosquitto/config/mosquitto.conf"

install -m 0755 files/firstboot.sh          "${ROOTFS_DIR}/usr/local/sbin/ftw-firstboot"
install -m 0644 files/firstboot.service     "${ROOTFS_DIR}/etc/systemd/system/ftw-firstboot.service"

on_chroot << 'EOF'
systemctl enable ftw-firstboot.service
EOF
