#!/usr/bin/env python3
"""Exercise an encrypted, online SQLite backup against a disposable server.

Usage: python3 contract-tests/restore_drill.py /absolute/path/to/server-wrapper
The wrapper must exec the server and honor PORT, DATABASE_PATH, JWT_SECRET,
ALLOW_NEW_REGISTER and BASE_URL. Requires Python 3 and a local gpg executable.
"""

from contextlib import closing
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


EMAIL = "restore-drill@example.invalid"
PASSWORD = "restore-drill-password"
CREATED = int(time.time() * 1000) - 10000
DELETED = CREATED + 1000


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def request(base, method, path, payload=None, token=None):
    headers = {}
    data = None
    if payload is not None:
        headers["Content-Type"] = "application/json"
        data = json.dumps(payload).encode()
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(base + path, data=data, headers=headers, method=method)
    # Never consult proxy settings: all traffic stays on the disposable loopback server.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(req, timeout=5) as response:
            status, body = response.status, response.read()
    except urllib.error.HTTPError as error:
        status, body = error.code, error.read()
    return status, body


def json_request(base, method, path, payload=None, token=None, expected=200):
    status, body = request(base, method, path, payload, token)
    require(status == expected, f"{method} {path}: expected {expected}, got {status}: {body[:500]!r}")
    return json.loads(body) if body else None


def login(base, password=PASSWORD):
    result = json_request(base, "POST", "/auth", {"email": EMAIL, "password": password})
    require(isinstance(result, dict) and isinstance(result.get("token"), str) and result["token"],
            "login did not return a token")
    return result["token"]


class Server:
    def __init__(self, wrapper, runtime, database, secret):
        self.base = f"http://127.0.0.1:{free_port()}"
        env = os.environ.copy()
        env.update(PORT=self.base.rsplit(":", 1)[1], DATABASE_PATH=str(database),
                   DATABASE_DIALECT="sqlite", MAIL_PROVIDER="console", JWT_SECRET=secret,
                   ALLOW_NEW_REGISTER="true", BASE_URL=self.base)
        self.log = open(runtime / "server.log", "wb")
        try:
            self.process = subprocess.Popen([str(wrapper)], cwd=runtime, env=env,
                                            stdout=self.log, stderr=subprocess.STDOUT,
                                            start_new_session=True)
        except BaseException:
            self.log.close()
            raise
        try:
            deadline = time.monotonic() + 30
            while time.monotonic() < deadline:
                require(self.process.poll() is None, "server exited during startup")
                try:
                    if request(self.base, "GET", "/")[0] == 200:
                        return
                except (OSError, urllib.error.URLError):
                    pass
                time.sleep(0.1)
            raise RuntimeError("server did not become ready within 30 seconds")
        except BaseException:
            self.close()
            raise

    def close(self):
        # The wrapper must exec; signal only its newly created process group.
        try:
            os.killpg(self.process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(self.process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            self.process.wait()
        self.log.close()

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()


def manga(number, title):
    return {"manga_id": number, "title": title, "alt_title": None,
            "url": "/manga/restore-drill", "public_url": "https://example.invalid/manga/restore-drill",
            "rating": 0.5, "content_rating": None, "cover_url": "https://example.invalid/cover.jpg",
            "large_cover_url": "", "tags": [], "state": None, "author": None,
            "source": "EXAMPLE", "nsfw": False}


def history(number, title, deleted=0):
    return {"manga_id": number, "manga": manga(number, title), "created_at": CREATED,
            "updated_at": CREATED, "chapter_id": 77, "page": 3, "scroll": 0.5,
            "percent": 0.25, "chapters": 12, "deleted_at": deleted}


def package(history_rows=None, categories=None, favourites=None):
    return {"history": history_rows, "categories": categories, "favourites": favourites,
            "timestamp": int(time.time() * 1000)}


def upload(base, path, token, payload):
    status, body = request(base, "POST", path, payload, token)
    require(status in (200, 204), f"upload {path}: unexpected status {status}: {body[:500]!r}")
    if status == 204:
        require(not body, f"upload {path}: 204 response has a body")


def assert_rows(result, field, key, expected):
    require(isinstance(result, dict) and isinstance(result.get(field), list), f"missing {field} list")
    rows = result[field]
    require(len(rows) == len(expected), f"unexpected number of {field} rows: {len(rows)}")
    indexed = {row[key]: row for row in rows}
    require(len(indexed) == len(rows) and set(indexed) == set(expected), f"unexpected {field} IDs")
    for identifier, values in expected.items():
        row = indexed[identifier]
        for name, value in values.items():
            require(row.get(name) == value, f"{field} {identifier}: unexpected {name}")
        if "manga" in row:
            require(row["manga"]["manga_id"] == identifier, f"{field}: manga ID mismatch")


def run(wrapper, root):
    live, archive, restored = (root / name for name in ("live", "archive", "restored"))
    for directory in (live, archive, restored):
        directory.mkdir()
    gpg_home = root / "gpg-home"
    gpg_home.mkdir(mode=0o700)
    secret = secrets.token_urlsafe(48)
    passphrase = secrets.token_urlsafe(48)
    db = live / "database.sqlite"
    snapshot = archive / "snapshot.sqlite"
    encrypted = archive / "backup.sqlite.gpg"
    recovered = restored / "database.sqlite"
    gpg = ["gpg", "--no-options", "--homedir", str(gpg_home), "--batch", "--yes",
           "--no-symkey-cache", "--pinentry-mode", "loopback", "--passphrase-fd", "0"]

    with Server(wrapper, live, db, secret) as server:
        token = login(server.base)
        rows = [history(810001, "Restore active"), history(810002, "Restore deleted", DELETED)]
        categories = [
            {"category_id": 8101, "created_at": CREATED, "sort_key": 1, "track": True,
             "title": "Restore shelf", "order": "MANUAL", "deleted_at": 0, "show_in_lib": True},
            {"category_id": 8102, "created_at": CREATED, "sort_key": 2, "track": True,
             "title": "Deleted shelf", "order": "MANUAL", "deleted_at": DELETED, "show_in_lib": True},
        ]
        favourites = [
            {"manga_id": 810001, "manga": manga(810001, "Restore active"), "category_id": 8101,
             "sort_key": 1, "pinned": True, "created_at": CREATED, "deleted_at": 0},
            {"manga_id": 810002, "manga": manga(810002, "Restore deleted"), "category_id": 8102,
             "sort_key": 2, "pinned": True, "created_at": CREATED, "deleted_at": DELETED},
        ]
        upload(server.base, "/resource/history", token, package(history_rows=rows))
        upload(server.base, "/resource/favourites", token, package(categories=categories, favourites=favourites))
        require(db.is_file(), "server did not create the configured SQLite database")
        # SQLite's online backup includes committed WAL pages and yields a consistent snapshot.
        with closing(sqlite3.connect(db.as_uri() + "?mode=ro", uri=True)) as source:
            with closing(sqlite3.connect(snapshot)) as destination:
                source.backup(destination)
        subprocess.run(gpg + ["--symmetric", "--cipher-algo", "AES256", "--output", str(encrypted),
                              str(snapshot)], input=(passphrase + "\n").encode(), check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        snapshot.unlink()
        # This committed write occurs strictly after the snapshot and must not survive restore.
        upload(server.base, "/resource/history", token,
               package(history_rows=[history(810003, "Post-backup marker")]))
        live_history = json_request(server.base, "GET", "/resource/history", token=token)
        require(any(row.get("manga_id") == 810003 for row in live_history["history"]),
                "post-backup marker was not committed to the live server")

    shutil.rmtree(live)  # The original runtime, including its WAL, is unavailable.
    restore_started = time.monotonic()
    require(encrypted.is_file() and encrypted.stat().st_size > 0, "encrypted backup missing")
    subprocess.run(gpg + ["--output", str(recovered), "--decrypt", str(encrypted)],
                   input=(passphrase + "\n").encode(), check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    with closing(sqlite3.connect(recovered.as_uri() + "?mode=ro", uri=True)) as conn:
        require(conn.execute("PRAGMA integrity_check").fetchall() == [("ok",)], "integrity check failed")
        require(not conn.execute("PRAGMA foreign_key_check").fetchall(), "foreign key check failed")

    with Server(wrapper, restored, recovered, secret) as server:
        status, _ = request(server.base, "POST", "/auth", {"email": EMAIL, "password": "wrong-password"})
        require(status == 400, "restored account did not reject an incorrect password")
        token = login(server.base)
        result = json_request(server.base, "POST", "/resource/history", package(history_rows=[]), token)
        assert_rows(result, "history", "manga_id", {
            810001: {"created_at": CREATED, "updated_at": CREATED, "deleted_at": 0, "page": 3},
            810002: {"created_at": CREATED, "updated_at": CREATED, "deleted_at": DELETED, "page": 3},
        })
        json_request(server.base, "GET", "/resource/favourites", token=token)
        result = json_request(server.base, "POST", "/resource/favourites",
                              package(categories=[], favourites=[]), token)
        assert_rows(result, "categories", "category_id", {
            8101: {"created_at": CREATED, "deleted_at": 0, "title": "Restore shelf"},
            8102: {"created_at": CREATED, "deleted_at": DELETED, "title": "Deleted shelf"},
        })
        assert_rows(result, "favourites", "manga_id", {
            810001: {"category_id": 8101, "created_at": CREATED, "deleted_at": 0, "pinned": True},
            810002: {"category_id": 8102, "created_at": CREATED, "deleted_at": DELETED, "pinned": True},
        })
    return time.monotonic() - restore_started


def main():
    if len(sys.argv) != 2:
        raise SystemExit(__doc__)
    wrapper = Path(sys.argv[1])
    if not wrapper.is_absolute() or not wrapper.is_file() or not os.access(wrapper, os.X_OK):
        raise SystemExit("wrapper must be an absolute path to an executable file")
    with tempfile.TemporaryDirectory(prefix="kotatsu-restore-drill-") as directory:
        elapsed = run(wrapper, Path(directory))
    print(f"Restored encrypted snapshot and completed client sync in {elapsed:.2f}s")


if __name__ == "__main__":
    main()
