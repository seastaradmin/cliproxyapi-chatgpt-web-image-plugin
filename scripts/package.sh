#!/bin/sh
set -eu

project_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_dir"
version=$(cat VERSION)
target_os=${GOOS:-$(go env GOOS)}
target_arch=${GOARCH:-$(go env GOARCH)}
case "$target_os" in darwin) extension=dylib ;; linux) extension=so ;; *) echo 'Unsupported target OS' >&2; exit 1 ;; esac
bundle=chatgpt-web-image_${version}_${target_os}_${target_arch}
stage_dir=$(mktemp -d)
trap 'rm -rf "$stage_dir"' EXIT HUP INT TERM
mkdir -p "$stage_dir/$bundle" dist
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o "$stage_dir/$bundle/chatgpt-web-image-v${version}.${extension}" .
rm "$stage_dir/$bundle/chatgpt-web-image-v${version}.h"
for item in VERSION worker.py install.sh install-runtime.sh README.md README_CN.md LICENSE; do
    cp "$item" "$stage_dir/$bundle/"
done
printf '%s/%s\n' "$target_os" "$target_arch" > "$stage_dir/$bundle/PLATFORM"
tar -czf "dist/$bundle.tar.gz" -C "$stage_dir" "$bundle"
printf 'Package: dist/%s.tar.gz\n' "$bundle"
