# ChatGPT Web image plugin

Provides `gpt-image-2-web` through CPA's `POST /v1/images/generations` and `POST /v1/images/edits`, using existing active, unexpired **free Codex OAuth** credentials from CPA's auth callbacks. Supports text-to-image and multiple reference images. No changes to CPA's normal Codex executor or auth files are required.

The image is generated through ChatGPT Web. `gpt-image-2-web` is an adapter alias: the web service chooses its underlying image model and does not independently attest a specific version. Web access and image quota remain account-dependent.

## Install

Requires Git, uv, and network access to ChatGPT. The installer downloads Python 3.13 and the locked dependencies of [chatgpt2api](https://github.com/basketikun/chatgpt2api) at commit `95c8024f2884a620f9b36d2d2cb504b590ce82d9`. Upstream source and license remain in the runtime's `upstream/` directory.

Download a matching macOS/Linux arm64/amd64 package from [Releases](https://github.com/seastaradmin/cliproxyapi-chatgpt-web-image-plugin/releases), verify it against that release's `SHA256SUMS`, then extract it. Install Git and [uv](https://docs.astral.sh/uv/getting-started/installation/) first. Prebuilt packages do not require Go or a CPA source checkout.

From the extracted directory:

```sh
sh install.sh /absolute/path/to/CPA/plugins "$HOME/.local/share/cpa/chatgpt-web-image-0.4.0"
```

Merge the printed configuration into CPA's existing `plugins.configs`; do not replace other plugin entries. The installer also saves `cpa-config-snippet.yaml` inside the new runtime directory. The instance pins `store.version: "0.4.0"` and points `runtime_dir` to that directory.

The host needs ABI 1/schema 6, model routing, static executors, auth callbacks and `openai-image` support. Verified against `seastaradmin/CLIProxyAPI` on `enterprise-base`. Compatibility with every older CPA release is not implied. Installing only a dynamic library through a plugin store does not install the Python runtime; use the complete package above.

CPA reloads the configuration. Check the management plugin list for registration errors. To also route bare `gpt-image-2` image requests to this plugin, enable `route_gpt_image_2`. Other API protocols are never intercepted.

When upgrading, use a new versioned library filename and a new runtime directory. Change `runtime_dir` to the new directory after copying the library; never overwrite a loaded dynamic library in place. If the instance has `store.version` pinned, update that pin to the installed version too.

```sh
curl http://127.0.0.1:8889/v1/images/generations \
  -H "Authorization: Bearer $CPA_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-image-2-web","prompt":"A small blue circle on white","n":1,"response_format":"b64_json"}'
```

## Management page

After reloading, the CPA sidebar includes **ChatGPT 网页生图** (ChatGPT Web images). Refresh the management page if its menu list is cached. The plugin embeds its page and assets in the library; no separate frontend install is needed.

Enter a CPA API key from API key management, describe the image, and optionally add up to 10 reference files. The page preserves reference order, previews inputs, and displays the original output dimensions with a download button. The key is used only in current page memory for same-origin `/v1/images/*` requests; it is never persisted or included in a URL. Refreshing clears it. Use an API key, not the management password. The public plugin resource routes serve static assets only and cannot read credentials or generate images.

## Reference images

Upload one or more files with repeated `image[]` fields. Reference order is preserved. `n=1` controls the number of **output** images, not reference images.

```sh
curl http://127.0.0.1:8889/v1/images/edits \
  -H "Authorization: Bearer $CPA_API_KEY" \
  -F 'model=gpt-image-2-web' \
  -F 'prompt=Place the subject from image 1 on the left and image 2 on the right.' \
  -F 'image[]=@reference-1.png' \
  -F 'image[]=@reference-2.png' \
  -F 'n=1'
```

For JSON clients, use `images` with data URLs:

```json
{
  "model": "gpt-image-2-web",
  "prompt": "Combine the subjects from both references.",
  "images": [
    {"image_url": "data:image/png;base64,..."},
    {"image_url": "data:image/jpeg;base64,..."}
  ],
  "n": 1
}
```

The plugin also accepts `images` as an array of data URL/base64 strings, or `image` for a single reference. It does not fetch remote URLs or read client-supplied local paths. Reference files are uploaded to the selected ChatGPT account as part of the generation request.

## Behavior and limits

- One non-streaming output image per request. Up to 10 reference images, PNG/JPEG/WebP, at most 10 MiB each and 40 million pixels each. The request and its normalized JSON representation must each fit within 32 MiB; base64 adds about one third to the image size. Unsupported options, including masks, are rejected before generation.
- `size` and `quality` are web prompt hints; exact output dimensions are not guaranteed. A live free-account request for `2048x2048` with `quality=high` returned `1254x1254`; this route does not currently guarantee native 2K or 4K output. The plugin does not resize or upscale results. `response_format=url` returns a data URI, not a hosted URL.
- Accounts rotate with one in-flight request per account. Paid, expired, disabled and unavailable credentials are excluded. CPA remains responsible for refreshing OAuth tokens. The worker confirms the web account is free and checks quota before generating.
- Each request starts an ephemeral Python worker bound to `127.0.0.1`, authenticated with a random per-request key. The ChatGPT access token travels through stdin only. No token is placed in command arguments, environment variables, plugin configuration or a separate account pool.
- The plugin invokes the loopback worker through `host.http.do`, so CPA handles client cancellation and logs the image API request/result. Actual ChatGPT HTTP exchanges use the Python library's browser-compatible TLS transport; those exchanges do not appear individually in CPA's transport logs.
- The worker exits after one authenticated request. Cancellation or plugin shutdown kills and reaps it; parent death also stops it. There is no separately managed server or post-connection generation deadline. Configure any global CPA proxy to bypass loopback. Set this plugin's `proxy_url` for ChatGPT egress; a credential-specific proxy takes precedence.
- No automatic generation retry or quota reset is attempted. A failure can occur after upstream generation starts; retrying may create another image. A later client request selects the next available account. Remote conversations remain in the originating account; generated image bytes are returned in memory without the upstream library's disk image cache.

## Verification

```sh
go test -race ./...
python3 -m unittest discover -p 'test_worker.py'
```

Use the installed runtime's Python for tests requiring Pillow. For a live check, use a separate CPA instance with a temporary config and one free credential, then verify the response contains a decodable image. A model-list entry alone does not prove image generation works.

## Build independently

The wire contract snapshot under `internal/cpasdk` is MIT-licensed and records its CPA provenance. There is no local `replace` directive or dependency on a full CPA checkout.

```sh
# Requires Go 1.26+ and a C compiler.
go test -race ./...
sh scripts/package.sh
# Install the resulting dist/*.tar.gz using the steps above.
```

[中文安装说明](README_CN.md)
