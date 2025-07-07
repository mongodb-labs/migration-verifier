#!/bin/sh

commit=$(git show --no-patch --format='%H')
release=$(git describe --tags --exact-match || echo "DEVELOPMENT")
buildTime=$(date -u)

printf 'Building migration-verifier …\n'
printf '\trelease: %s\n' "$release"
printf '\tcommit: %s\n' "$commit"
printf '\tbuildTime: %s\n' "$buildTime"

go build -ldflags="-X 'main.Revision=$commit' -X 'main.Release=$release' -X 'main.BuildTime=$buildTime'" main/migration_verifier.go
