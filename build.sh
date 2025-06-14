#!/bin/sh

commit=$(git show --no-patch --format='%H')
buildTime=$(date -u)

printf 'Building migration-verifier …\n\tcommit: %s\n\tbuildTime: %s\n' "$commit" "$buildTime"

go build -ldflags="-X 'main.Revision=$commit' -X 'main.BuildTime=$buildTime'" main/migration_verifier.go
