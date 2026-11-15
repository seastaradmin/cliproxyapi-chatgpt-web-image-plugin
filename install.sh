#!/bin/sh
set -eu
umask 077

source_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
plugins_dir=${1:?Usage: install.sh /absolute/CPA/plugins /absolute/new/runtime}
runtime_dir=${2:?Usage: install.sh /absolute/CPA/plugins /absolute/new/runtime}
case "$plugins_dir:$runtime_dir" in /*:/*) ;; *) echo 'Both paths must be absolute' >&2; exit 1 ;; esac
version=$(cat "$source_dir/VERSION")
case $(uname -s) in Darwin) platform=darwin; extension=dylib ;; Linux) platform=linux; extension=so ;; *) echo 'Only macOS and Linux are supported' >&2; exit 1 ;; esac
case $(uname -m) in arm64|aarch64) architecture=arm64 ;; x86_64|amd64) architecture=amd64 ;; *) echo 'Unsupported CPU architecture' >&2; exit 1 ;; esac
if [ "$(cat "$source_dir/PLATFORM")" != "$platform/$architecture" ]; then
    echo 'Download the release package matching this operating system and CPU' >&2
    exit 1
fi
library=chatgpt-web-image-v${version}.${extension}
test -f "$source_dir/$library"
if [ -e "$plugins_dir/$library" ] || [ -e "$runtime_dir" ]; then
    echo 'This plugin version or runtime already exists; choose a fresh installation' >&2
    exit 1
fi
sh "$source_dir/install-runtime.sh" "$runtime_dir"
mkdir -p "$plugins_dir"
cp "$source_dir/$library" "$plugins_dir/$library"
"$runtime_dir/.venv/bin/python" - "$runtime_dir" "$version" <<'PY'
import json, sys
from pathlib import Path
runtime, version = sys.argv[1:]
snippet = f'''# Merge into the existing CPA plugins mapping.
plugins:
  enabled: true
  configs:
    chatgpt-web-image:
      enabled: true
      store:
        version: {json.dumps(version)}
      runtime_dir: {json.dumps(runtime)}
      route_gpt_image_2: false
      proxy_url: ""
'''
path = Path(runtime) / 'cpa-config-snippet.yaml'
path.write_text(snippet)
print(f'Installed. Merge this configuration into CPA: {path}')
print(snippet)
PY
