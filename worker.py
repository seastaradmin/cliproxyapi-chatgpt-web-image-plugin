"""One-request ChatGPT Web worker. Credentials arrive only over stdin."""

import base64
import contextlib
import hmac
import io
import json
import logging
import os
from pathlib import Path
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

MAX_REQUEST = 32 * 1024 * 1024
MAX_IMAGE_BYTES = 10 * 1024 * 1024
MAX_REFERENCE_IMAGES = 10


class GenerationError(Exception):
    def __init__(self, status, code):
        self.status, self.code = status, code


def memory_image_result(items, prompt, response_format, base_url=None, created=None, **_):
    # The upstream formatter saves every result to disk, even for b64_json.
    return {"created": created or int(time.time()), "data": [
        {"b64_json": item["b64_json"]} for item in items if item.get("b64_json")
    ]}


def load_backend(proxy_url):
    logging.disable(logging.CRITICAL)
    os.environ["STORAGE_BACKEND"] = "json"
    # Required by the imported library; its HTTP server is never started.
    os.environ["CHATGPT2API_AUTH_KEY"] = "cpa-private-worker-unused-auth-key"
    sys.path.insert(0, str(Path(__file__).resolve().parent / "upstream"))
    from services.config import config

    config.data.update({
        "proxy": proxy_url,
        "auto_remove_invalid_accounts": False,
        "auto_remove_rate_limited_accounts": False,
        "auto_relogin_after_refresh": False,
        "image_parallel_generation": False,
        "image_remove_conversation_after_result": False,
        "image_remove_conversation_always": False,
        "image_storage": {"enabled": False},
        "backup": {"enabled": False},
        "proxy_runtime": {"enabled": False, "clearance": {"enabled": False}},
    })
    # CPA cancellation kills the worker; no deadline after upstream connection.
    type(config).image_poll_timeout_secs = property(lambda self: float("inf"))
    from utils.log import logger
    logger._logger.disabled = True
    from services.openai_backend_api import OpenAIBackendAPI, InvalidAccessTokenError, ImageContentPolicyError
    from services.protocol import conversation
    from utils.helper import iter_sse_payloads, UpstreamHTTPError

    conversation.format_image_result = memory_image_result

    def uncapped(self, response, _cap):
        try:
            yield from iter_sse_payloads(response)
        finally:
            response.close()

    OpenAIBackendAPI._iter_sse_payloads_capped = uncapped
    return OpenAIBackendAPI, conversation, (InvalidAccessTokenError, ImageContentPolicyError, UpstreamHTTPError)


def generate(bootstrap, request):
    images = checked_references(request.get("images", []))
    # Imported libraries may print storage initialization; never use worker stdout.
    with open(os.devnull, "w") as sink, contextlib.redirect_stdout(sink), contextlib.redirect_stderr(sink):
        backend_class, conversation, errors = load_backend(bootstrap.get("proxy_url", ""))
        invalid_token, policy_error, http_error = errors
        try:
            with backend_class(bootstrap["access_token"]) as backend:
                original_request = backend.session.request

                def without_deadline(*args, **kwargs):
                    kwargs["timeout"] = None
                    return original_request(*args, **kwargs)

                backend.session.request = without_deadline
                info = backend.get_user_info()
                if info.get("type") != "free":
                    raise GenerationError(403, "invalid_credential")
                if info.get("quota") == 0:
                    raise GenerationError(429, "image_quota_exhausted")
                req = conversation.ConversationRequest(
                    prompt=request["prompt"], model="gpt-image-2", n=1,
                    images=images,
                    size=request.get("size") or "1024x1024",
                    quality=request.get("quality") or "auto",
                    response_format="b64_json", message_as_error=True,
                )
                for output in conversation.stream_image_outputs(backend, req, 1, 1):
                    if output.kind != "result":
                        continue
                    items = output.data or []
                    if len(items) != 1 or not items[0].get("b64_json"):
                        raise GenerationError(502, "no_image_generated")
                    return checked_result(items[0]["b64_json"], request.get("response_format"))
                raise GenerationError(502, "no_image_generated")
        except invalid_token:
            raise GenerationError(401, "invalid_credential") from None
        except policy_error:
            raise GenerationError(400, "upstream_rejected") from None
        except http_error as exc:
            status = getattr(exc, "status_code", 502)
            if status == 401:
                raise GenerationError(401, "invalid_credential") from None
            if status == 429:
                raise GenerationError(429, "upstream_rejected") from None
            raise GenerationError(502, "upstream_rejected") from None


def checked_references(images):
    from PIL import Image
    if not isinstance(images, list) or len(images) > MAX_REFERENCE_IMAGES:
        raise GenerationError(400, "invalid_request_error")
    for value in images:
        try:
            if not isinstance(value, str) or not value.startswith("data:"):
                raise ValueError("expected data URL")
            header, encoded = value.split(",", 1)
            if header not in {"data:image/png;base64", "data:image/jpeg;base64", "data:image/webp;base64"}:
                raise ValueError("unsupported image type")
            data = base64.b64decode(encoded, validate=True)
            if len(data) > MAX_IMAGE_BYTES:
                raise ValueError("image too large")
            with Image.open(io.BytesIO(data)) as image:
                if image.width * image.height > 40_000_000:
                    raise ValueError("image dimensions too large")
                if header != "data:" + Image.MIME.get(image.format, "") + ";base64":
                    raise ValueError("image MIME mismatch")
                image.verify()
        except Exception:
            raise GenerationError(400, "invalid_request_error") from None
    return images


def checked_result(encoded, response_format):
    from PIL import Image
    data = base64.b64decode(encoded, validate=True)
    with Image.open(io.BytesIO(data)) as image:
        image.verify()
        mime = Image.MIME.get(image.format)
    if mime not in {"image/png", "image/jpeg", "image/webp"}:
        raise GenerationError(502, "no_image_generated")
    item = {"url": "data:" + mime + ";base64," + encoded} if response_format == "url" else {"b64_json": encoded}
    return {"created": int(time.time()), "data": [item]}


def handler_class(bootstrap, generate_fn=generate):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            expected = "Bearer " + bootstrap["worker_key"]
            if not hmac.compare_digest(self.headers.get("Authorization", ""), expected):
                self.reply(401, {"error": {"code": "invalid_credential"}})
                return
            try:
                if self.path != "/generate":
                    raise GenerationError(404, "invalid_request_error")
                length = int(self.headers.get("Content-Length", "0"))
                if not 0 < length <= MAX_REQUEST:
                    raise GenerationError(400, "invalid_request_error")
                request = json.loads(self.rfile.read(length))
                if not isinstance(request, dict) or not isinstance(request.get("prompt"), str) or not request["prompt"].strip():
                    raise GenerationError(400, "invalid_request_error")
                result = generate_fn(bootstrap, request)
                self.reply(200, result)
            except GenerationError as exc:
                self.reply(exc.status, {"error": {"code": exc.code}})
            except Exception:
                self.reply(502, {"error": {"code": "generation_failed"}})
            finally:
                threading.Thread(target=self.server.shutdown, daemon=True).start()

        def reply(self, status, result):
            body = json.dumps(result).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    return Handler


def watch_parent(parent_pid):
    while os.getppid() == parent_pid:
        time.sleep(1)
    os._exit(0)


def main():
    bootstrap = json.loads(sys.stdin.buffer.readline(MAX_REQUEST + 1))
    if not bootstrap.get("access_token") or len(bootstrap.get("worker_key", "")) != 64:
        return 1
    threading.Thread(target=watch_parent, args=(os.getppid(),), daemon=True).start()
    with HTTPServer(("127.0.0.1", 0), handler_class(bootstrap)) as server:
        print(json.dumps({"port": server.server_port}), flush=True)
        server.serve_forever()
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception:
        sys.exit(1)
