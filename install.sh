#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
prefix=${PREFIX:-"$HOME/.local"}
mkdir -p "$prefix/lib/looper/builds" "$prefix/bin"
prefix=$(CDPATH= cd -- "$prefix" && pwd)

if [ -e "$prefix/bin/looper" ] && [ ! -L "$prefix/bin/looper" ]; then
    printf '%s\n' "Refusing to replace a regular executable: $prefix/bin/looper" >&2
    exit 1
fi

build_dir=$(mktemp -d "$prefix/lib/looper/builds/build.XXXXXX")
(
    cd "$repo_dir"
    GOWORK=off go build -trimpath -o "$build_dir/looper" .
)
"$build_dir/looper" version

# Keep previous builds available to running controllers.
ln -s "$build_dir/looper" "$build_dir/looper-link"
mv -Tf "$build_dir/looper-link" "$prefix/bin/looper"
printf 'Installed %s\n' "$prefix/bin/looper"
