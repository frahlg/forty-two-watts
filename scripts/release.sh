#!/bin/bash
# Build static binaries and create a GitHub release
# Usage: ./scripts/release.sh v0.2.0

set -euo pipefail

VERSION=${1:?Usage: $0 <version>}
REPO="frahlg/forty-two-watts"

echo "Building forty-two-watts ${VERSION}..."
mkdir -p release

# Build static binaries for both architectures
for PLATFORM in linux/arm64 linux/amd64; do
    ARCH=$(echo $PLATFORM | cut -d/ -f2)
    echo "  Building ${ARCH}..."
    docker build --platform ${PLATFORM} -t forty-two-watts:${ARCH} .
    docker create --name ems-extract forty-two-watts:${ARCH}
    docker cp ems-extract:/app/forty-two-watts release/forty-two-watts-linux-${ARCH}
    docker rm ems-extract
    chmod +x release/forty-two-watts-linux-${ARCH}
    tar czf release/forty-two-watts-linux-${ARCH}.tar.gz \
        -C release forty-two-watts-linux-${ARCH} \
        -C .. drivers/ web/ config.example.yaml
done

echo "Creating GitHub release ${VERSION}..."
gh release create ${VERSION} \
    release/forty-two-watts-linux-arm64.tar.gz \
    release/forty-two-watts-linux-amd64.tar.gz \
    --repo ${REPO} \
    --title "${VERSION}" \
    --generate-notes

echo "Done! Release: https://github.com/${REPO}/releases/tag/${VERSION}"
