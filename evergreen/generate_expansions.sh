set -o errexit
set -o xtrace

cd "$verifier_dir" || exit 1

export GOPATH="$(cd "$(pwd)/../../../.." && pwd)"
if [ "Windows_NT" = "$OS" ]; then
  export GOPATH="$(cygpath -w "$GOPATH")"
fi;

# Set the PATH for this variant
export PATH="$GOPATH/bin:$PATH"
export VERIFIER_BUILD_TAGS="$BUILD_TAGS"

# We get the raw version string (r1.2.3-45-gabcdef) from git.
export VERIFIER_VERSION="$(git describe)"
# If this is a patch build, we add the patch version id to the version string so we know
# this build was a patch, and which evergreen task it came from.
if [ "$IS_PATCH" = "true" ]; then
  export VERIFIER_VERSION="$VERIFIER_VERSION-patch-$VERSION_ID"
fi

cat <<EOT > verifier_expansion.yml
VERIFIER_VERSION: "$VERIFIER_VERSION"
PREPARE_SHELL: |
  set -o errexit
  set -o xtrace

  export GOPATH="$GOPATH"

  if [ "Windows_NT" = "$OS" ]; then
    export GOOS="windows"
    export GOROOT="c:/golang/go1.19"
    export GOCACHE="C:/windows/temp"
    export PATH="/cygdrive/c/golang/go1.19/bin:/cygdrive/c/mingw-w64/x86_64-4.9.1-posix-seh-rt_v3-rev1/mingw64/bin:/cygdrive/c/sasl/:$PATH"
  else
    export GOROOT=/opt/golang/go1.19
    export PATH="/opt/golang/go1.19/bin:$PATH"
  fi;

  # Set OS-level default Go configuration
  UNAME_S=$(PATH="/usr/bin:/bin" uname -s)
  # Set OS-level compilation flags
  if [ "$UNAME_S" = CYGWIN* ]; then
    export CGO_CFLAGS="-D_WIN32_WINNT=0x0A00 -DNTDDI_VERSION=0x0A000000"
    export GOCACHE="C:/windows/temp"
  elif [ "$UNAME_S" = Darwin ]; then
    export CGO_CFLAGS="-mmacosx-version-min=10.15"
    export CGO_LDFLAGS="-mmacosx-version-min=10.15"
  fi

  export VERIFIER_BUILD_TAGS="$VERIFIER_BUILD_TAGS"
  export VERIFIER_VERSION="$VERIFIER_VERSION"

  export AWS_ACCESS_KEY_ID='${release_aws_access_key_id}'
  export AWS_SECRET_ACCESS_KEY='${release_aws_secret}'

  export EVG_IS_PATCH='${is_patch}'
  export EVG_TRIGGERED_BY_TAG='${triggered_by_git_tag}'
  export EVG_BUILD_ID='${build_id}'
  export EVG_VERSION='${version_id}'
  export EVG_VARIANT='${build_variant}'
  if [ '${_platform}' != '' ]; then
    export EVG_VARIANT='${_platform}'
  fi
  export EVG_USER='${evg_user}'
  export EVG_KEY='${evg_key}'
  export PLATFORM='${_platform}'
  export ARCH='${_arch}'
  export OS='${_os}'
EOT
# See what we've done
cat verifier_expansion.yml
