#!/bin/sh

revision=$(git describe --tags --exact-match || echo "DEVELOPMENT:" "$(git show --no-patch --format='%H')")
buildTime=$(date -u)

goos=$(go env GOOS)
goarch=$(go env GOARCH)

printf 'Building migration-verifier for %s/%s …\n' "$goos" "$goarch"
printf '\trevision: %s\n' "$revision"
printf '\tbuildTime: %s\n' "$buildTime"

go build -ldflags="-X 'main.Revision=$revision' -X 'main.BuildTime=$buildTime'" main/migration_verifier.go
