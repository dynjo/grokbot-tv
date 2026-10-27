#!/bin/sh
# Build the daemon (static linux/arm64) and the overlay .ipk into build/.
set -e
cd "$(dirname "$0")/.."

mkdir -p build
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o build/grokbotd ./cmd/grokbotd
echo "built build/grokbotd"

rm -f build/*.ipk
npx -y -p @webos-tools/cli ares-package overlay -o build >/dev/null
ls build/*.ipk
