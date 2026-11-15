#!/bin/sh
set -eu
umask 077

source_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
runtime_dir=${1:?Usage: install-runtime.sh /absolute/runtime/directory}
upstream_commit=95c8024f2884a620f9b36d2d2cb504b590ce82d9
case "$runtime_dir" in /*) ;; *) echo 'Runtime path must be absolute' >&2; exit 1 ;; esac
if [ -e "$runtime_dir" ]; then
    echo 'Choose a new runtime directory; existing installations are never overwritten' >&2
    exit 1
fi
command -v uv >/dev/null
mkdir -p "$runtime_dir"
git init -q "$runtime_dir/upstream"
git -C "$runtime_dir/upstream" remote add origin https://github.com/basketikun/chatgpt2api.git
git -C "$runtime_dir/upstream" fetch --quiet --depth 1 origin "$upstream_commit"
git -C "$runtime_dir/upstream" checkout --quiet --detach FETCH_HEAD
test "$(git -C "$runtime_dir/upstream" rev-parse HEAD)" = "$upstream_commit"
UV_PROJECT_ENVIRONMENT="$runtime_dir/.venv" uv sync --project "$runtime_dir/upstream" --frozen --no-dev --python 3.13
cp "$source_dir/worker.py" "$runtime_dir/worker.py"
printf '%s\n' "$upstream_commit" > "$runtime_dir/UPSTREAM_COMMIT"
printf 'Runtime installed: %s\n' "$runtime_dir"
