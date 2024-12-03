#!/bin/sh

commit=$(git show --no-patch --format='%H')

go build -ldflags="-X 'main.Revision=$commit'" main/migration_verifier.go
