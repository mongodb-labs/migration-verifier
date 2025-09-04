#!/bin/sh

set -o errexit

filename=migration_verifier

RELEASE_URL="https://api.github.com/repos/mongodb-labs/migration-verifier/releases/latest"

OS=$(uname -o | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
if [ "$ARCH" = "aarch64" ]; then
    ARCH=arm64
fi

echo "Looks like you’re running $OS on $ARCH."

MANIFEST=$(wget -qO- "$RELEASE_URL")

VERSION=$(printf "%s" "$MANIFEST" | jq -r .name)

echo "Latest release: $VERSION"

ALL_URLS=$(printf "%s" "$MANIFEST" \
    | jq -r '.assets[] | .browser_download_url' \
)

DOWNLOAD_URL=$(echo "$ALL_URLS" \
    | grep "_${OS}_" | grep "_$ARCH" ||: \
)

if [ -z "$DOWNLOAD_URL" ]; then
    echo >&2 "No download URL found for $OS/$ARCH:"
    echo >&2 "$ALL_URLS"
    exit 1
fi

echo "Downloading $DOWNLOAD_URL …"

wget -O "$filename" "$DOWNLOAD_URL"

chmod +x "$filename"

echo "Migration Verifier $VERSION is now saved as $filename."
