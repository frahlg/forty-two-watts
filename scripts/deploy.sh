#!/bin/bash
# Deploy latest release to a remote host
# Usage: ./scripts/deploy.sh homelab-rpi [version]

set -euo pipefail

HOST=${1:?Usage: $0 <ssh-host> [version]}
VERSION=${2:-latest}
REPO="frahlg/forty-two-watts"
REMOTE_DIR="forty-two-watts-app"

# Detect architecture
ARCH=$(ssh ${HOST} "uname -m")
case ${ARCH} in
    aarch64) BINARY="forty-two-watts-linux-arm64" ;;
    x86_64)  BINARY="forty-two-watts-linux-amd64" ;;
    *) echo "Unsupported arch: ${ARCH}"; exit 1 ;;
esac

# Get download URL
if [ "${VERSION}" = "latest" ]; then
    URL=$(gh release view --repo ${REPO} --json assets --jq ".assets[] | select(.name | contains(\"${BINARY}\")) | .url")
else
    URL=$(gh release view ${VERSION} --repo ${REPO} --json assets --jq ".assets[] | select(.name | contains(\"${BINARY}\")) | .url")
fi

echo "Deploying ${BINARY} to ${HOST}..."

ssh ${HOST} "
    mkdir -p ~/${REMOTE_DIR}
    cd ~/${REMOTE_DIR}

    # Stop running instance
    pkill -f forty-two-watts 2>/dev/null || true
    sleep 1

    # Download and extract
    curl -sL '${URL}' | tar xz
    chmod +x ${BINARY}

    # Update drivers and web from tarball
    echo 'Binary updated to ${VERSION}'

    # Start
    nohup ./${BINARY} config.yaml > forty-two-watts.log 2>&1 &
    sleep 2

    # Verify
    if curl -sf http://localhost:8080/api/health > /dev/null; then
        echo 'Deployed and running!'
    else
        echo 'WARNING: health check failed'
        tail -5 forty-two-watts.log
    fi
"
