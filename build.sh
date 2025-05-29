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

while getopts "o:a:" option; do
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

commit=$(git show --no-patch --format='%H')
buildTime=$(date -u)

GOARCH=${arch:=$(go env GOARCH)} GOOS=${os:=$(go env GOOS)} go build -ldflags="-X 'main.Revision=$commit' -X 'main.BuildTime=$buildTime'" main/migration_verifier.go
