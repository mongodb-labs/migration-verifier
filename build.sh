#!/bin/sh

revision=$(git describe --tags --exact-match || git show --no-patch --format='%H')
buildTime=$(date -u)

printf 'Building migration-verifier …\n\tcommit: %s\n\tbuildTime: %s\n' "$revision" "$buildTime"

go build -ldflags="-X 'main.Revision=$revision' -X 'main.BuildTime=$buildTime'" main/migration_verifier.go
