#!/bin/bash -e
# Runs inside the pi-gen chroot with apt + systemd available. This is
# where the image is turned into a forty-two-watts deployment —
# Docker installed, compose file placed, firstboot service enabled.

# Docker via the official convenience script. Same install path as
# scripts/install.sh so bare-metal and image installs converge on the
# same engine version, and users moving between the two don't hit
# packaging surprises.
curl -fsSL https://get.docker.com | sh

systemctl enable docker.service
systemctl enable avahi-daemon.service

# /etc/hosts entry prevents sudo's "unable to resolve host 42w"
# warning on first boot. pi-gen writes /etc/hostname from
# TARGET_HOSTNAME but leaves /etc/hosts at the stock Raspberry Pi OS
# template (which hard-codes `raspberrypi`).
sed -i 's/^127\.0\.1\.1.*/127.0.1.1\t42w/' /etc/hosts

# /home/ftw is the deploy directory — docker-compose.yml lives here
# and `docker compose up -d` is run from it on first boot. The data/
# subdirectory is chowned to 100:101 up-front because the in-container
# ftw user (alpine `adduser -S`) needs to own it before SQLite can
# create state.db. Same UID/GID mapping as scripts/install.sh.
install -d -m 0755                     /home/ftw
install -d -m 0755                     /home/ftw/forty-two-watts
install -d -m 0755 -o 100 -g 101       /home/ftw/forty-two-watts/data
install -d -m 0755                     /home/ftw/forty-two-watts/mosquitto
install -d -m 0755                     /home/ftw/forty-two-watts/mosquitto/config

install -m 0644 files/docker-compose.yml    /home/ftw/forty-two-watts/docker-compose.yml
install -m 0644 files/mosquitto.conf        /home/ftw/forty-two-watts/mosquitto/config/mosquitto.conf

install -m 0755 files/firstboot.sh          /usr/local/sbin/ftw-firstboot
install -m 0644 files/firstboot.service     /etc/systemd/system/ftw-firstboot.service

systemctl enable ftw-firstboot.service
