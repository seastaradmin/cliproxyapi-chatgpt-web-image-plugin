import base64
import http.client
import io
import json
import threading
import unittest
from types import SimpleNamespace
from unittest.mock import patch
from http.server import HTTPServer

import worker


class WorkerTests(unittest.TestCase):
    def test_authentication_errors_and_single_request_exit(self):
        calls = []

        def fake_generate(bootstrap, request):
            calls.append(request)
            raise RuntimeError("SECRET-UPSTREAM-TOKEN")

        server = HTTPServer(("127.0.0.1", 0), worker.handler_class({"worker_key": "k"}, fake_generate))
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        try:
            for key, status in [("wrong", 401), ("k", 502)]:
                connection = http.client.HTTPConnection("127.0.0.1", server.server_port)
                connection.request("POST", "/generate", json.dumps({"prompt": "circle"}), {"Authorization": "Bearer " + key})
                response = connection.getresponse()
                self.assertEqual(response.status, status)
                self.assertNotIn(b"SECRET", response.read())
                connection.close()
            thread.join(3)
            self.assertFalse(thread.is_alive())
            self.assertEqual(len(calls), 1)
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def test_result_requires_real_image_bytes(self):
        from PIL import Image
        buf = io.BytesIO()
        Image.new("RGB", (2, 2), "blue").save(buf, format="PNG")
        encoded = base64.b64encode(buf.getvalue()).decode()
        self.assertEqual(worker.checked_result(encoded, "b64_json")["data"], [{"b64_json": encoded}])
        self.assertTrue(worker.checked_result(encoded, "url")["data"][0]["url"].startswith("data:image/png;base64,"))
        with self.assertRaises(Exception):
            worker.checked_result(base64.b64encode(b"not an image").decode(), "b64_json")

    def test_formatter_does_not_store_images(self):
        result = worker.memory_image_result([{"b64_json": "bytes"}], "prompt", "b64_json")
        self.assertEqual(result["data"], [{"b64_json": "bytes"}])

    def test_reference_validation_blocks_paths_urls_and_corrupt_images(self):
        for images in [["/etc/passwd"], ["https://example.com/a.png"], ["data:image/png;base64,eA=="], ["x"] * 11]:
            with self.subTest(images=images), self.assertRaises(worker.GenerationError), patch.object(worker, "load_backend") as load:
                try:
                    worker.generate({"access_token": "unused"}, {"prompt": "Edit", "images": images})
                finally:
                    load.assert_not_called()

    def test_multiple_references_reach_web_request(self):
        from PIL import Image
        refs = []
        for color in ["blue", "orange"]:
            buf = io.BytesIO()
            Image.new("RGB", (2, 2), color).save(buf, format="PNG")
            refs.append("data:image/png;base64," + base64.b64encode(buf.getvalue()).decode())
        calls = []

        class Backend:
            def __init__(self, token):
                self.session = SimpleNamespace(request=lambda *args, **kwargs: None)

            def __enter__(self):
                return self

            def __exit__(self, *_):
                pass

            def get_user_info(self):
                return {"type": "free", "quota": 1}

        def outputs(backend, req, *_):
            calls.append(req)
            yield SimpleNamespace(kind="result", data=[{"b64_json": refs[0].split(",", 1)[1]}])

        class UpstreamError(Exception):
            pass

        conversation = SimpleNamespace(ConversationRequest=SimpleNamespace, stream_image_outputs=outputs)
        with patch.object(worker, "load_backend", return_value=(Backend, conversation, (UpstreamError,) * 3)):
            result = worker.generate({"access_token": "test"}, {"prompt": "Combine", "images": refs, "size": "2048x2048"})
        self.assertEqual(calls[0].images, refs)
        self.assertEqual(calls[0].size, "2048x2048")
        self.assertEqual(len(result["data"]), 1)


if __name__ == "__main__":
    unittest.main()
