#!/bin/sh

usage() {
cat << EOF
Usage: ./build.sh [-a <386|amd64|arm|arm64>] [-o <linux|darwin|windows>]
Build migration-verifier for mongosync.

-a          Specify the target architecture for the build, if not specified, it will use the current architecture.
-o          Specify the target operating system for the build, if not specified, it will use the current OS.
-h          Display this help message.
EOF
exit 1;
}

while getopts "o:a:h" option; do
    case "$option" in
        a)
            arch=${OPTARG}
            test "$arch" = "386" -o "$arch" = amd64 -o "$arch" = "arm" -o "$arch" = "arm64" || usage
            ;;
        o)
            os=${OPTARG}
            test "$os" = "linux" -o "$os" = "darwin" -o "$os" = "windows" || usage
            ;;
        h)
            usage
            ;;
        *)
            usage
            ;;
    esac
done
shift $((OPTIND-1))

revision=$(git describe --tags --exact-match 2>/dev/null || echo "DEVELOPMENT:$(git describe --tags)")
buildTime=$(date -u)

goos=$(go env GOOS)
goarch=$(go env GOARCH)

printf 'Building migration-verifier for %s/%s …\n' "$goos" "$goarch"
printf '\tRevision: %s\n' "$revision"
printf '\tBuild Time: %s\n' "$buildTime"

go build -ldflags="-X 'main.Revision=$revision' -X 'main.BuildTime=$buildTime'" main/migration_verifier.go
