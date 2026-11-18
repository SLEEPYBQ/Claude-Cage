from concurrent.futures import ThreadPoolExecutor, TimeoutError
import http.client
import importlib.util
import json
from pathlib import Path
import socket
import tempfile
from threading import Event, Thread
from unittest.mock import patch


def request(socket_path, method, path, payload=None):
    connection = http.client.HTTPConnection("localhost", timeout=4)
    connection.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    connection.sock.settimeout(4)
    connection.sock.connect(str(socket_path))
    try:
        encoded = None if payload is None else json.dumps(payload).encode()
        connection.request(method, path, body=encoded)
        response = connection.getresponse()
        return response.status, json.loads(response.read())
    finally:
        connection.close()


spec = importlib.util.spec_from_file_location("desktop_files", Path(__file__).with_name("desktop-files.py"))
files = importlib.util.module_from_spec(spec)
spec.loader.exec_module(files)

with tempfile.TemporaryDirectory(dir="/tmp") as temp:
    project = Path(temp) / "project"
    project.mkdir()
    spool = Path(temp) / "spool"
    spool.mkdir()
    socket_path = Path(temp) / "files.sock"
    delivered = []
    broker = files.Broker(spool, lambda _request, paths: delivered.append(paths))
    reading, release = Event(), Event()
    is_dir = Path.is_dir

    def stalled_is_dir(path):
        if path == project:
            reading.set()
            if not release.wait(3):
                raise RuntimeError("test directory read exceeded maximum wait")
        return is_dir(path)

    with files.FileServer(str(socket_path), files.Handler) as server:
        server.broker = broker
        worker = Thread(target=server.serve_forever, daemon=True)
        worker.start()
        try:
            def choose():
                key = broker.start("chooser", multiple=False, directory=True)["id"]
                return {"id": key, "path": str(project)}

            assert request(socket_path, "POST", "/directory", choose()) == (200, {"ok": True})
            assert delivered == [[str(project)]]
            print("normal /directory: HTTP 200 and directory delivered")

            payload = choose()
            with patch.object(Path, "is_dir", stalled_is_dir), ThreadPoolExecutor(max_workers=2) as pool:
                handoff = pool.submit(request, socket_path, "POST", "/directory", payload)
                try:
                    assert reading.wait(2), "receiver never attempted directory metadata read"
                    poll = pool.submit(request, socket_path, "GET", "/requests")
                    for description, pending in [("/directory handoff", handoff), ("/requests poll", poll)]:
                        try:
                            pending.result(timeout=0.25)
                        except TimeoutError:
                            print(f"{description}: no response while the directory metadata read was stalled")
                        else:
                            raise AssertionError(f"{description} unexpectedly returned")
                    assert delivered == [[str(project)]]
                finally:
                    release.set()
                assert handoff.result(timeout=2) == (200, {"ok": True})
                assert poll.result(timeout=2)[0] == 200
                assert delivered == [[str(project)], [str(project)]]
                print("after releasing the simulated read: both requests returned HTTP 200")
        finally:
            server.shutdown()
            worker.join()
