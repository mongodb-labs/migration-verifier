#!/bin/sh

commit=$(git show --no-patch --format='%H')
buildTime=$(date -u)

go build -ldflags="-X 'main.Revision=$commit' -X 'main.BuildTime=$buildTime'" main/migration_verifier.go
