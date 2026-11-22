# Live sync contract scenarios

Executable Given/When/Then stories exercise a real HTTP server. The compatibility suites use HTTP (and local SMTP for password recovery) without depending on the backend language or database.

| Behaviour | What it checks |
| --- | --- |
| Reading history | First upload, restore on an empty phone, newer reading progress, stale upload, and propagation of a deletion. |
| Favourites | Android-style GET before POST, restore on an empty phone, deletion seen by a stale phone, and a later re-add. |
| Offline and replay | Merge disjoint libraries, upload the same packet twice without duplicates, preserve old deletion markers and reconcile stale devices. |
| Account isolation | An unauthenticated request is rejected and another account cannot read the first account's sync data. |
| Android wire format | Leading double slash and the observed output for null versus empty manga cover fields. |
| Password recovery | Wrong password, delivery of a real reset email, successful reset, token reuse rejected, and login with the new password. |

The client flow is based on Kotatsu-Redo `9ef707775` (`SyncHelper.kt`). These stories cover the selected sync flows, not the entire API or local Android database behavior.

## Verify a backend

Use Go 1.24 or newer. The backend must have a **fresh, disposable database** and allow account registration via `/auth`. The runner sends real writes using fixed fixture data. Do not point it at a deployed instance or reuse the same database for another run.

```sh
cd contract-tests
# Sync, wire formatting and account isolation:
go run . verify --base-url http://127.0.0.1:8090

# Password recovery: start again with fresh storage and point the backend's SMTP at this Mailpit.
go run . verify --suite recovery --base-url http://127.0.0.1:8090 --mailpit-url http://127.0.0.1:8025
```

For the Ktor reference, build a local image from the separate [original server checkout](../experiments/README.md#original-ktor-baseline). Run the following from this repository's root:

```sh
podman build -t localhost/kotatsu-sync-contract /absolute/path/to/original/server
export REFERENCE_IMAGE=localhost/kotatsu-sync-contract
./contract-tests/reference.sh verify sync
./contract-tests/reference.sh verify recovery
./contract-tests/reference.sh verify observations
```

`reference.sh` uses Podman, curl, OpenSSL, and locally available images. `DB_IMAGE` defaults to `docker.io/library/mariadb:11.8`; recovery and observations also use `MAILPIT_IMAGE` (default `docker.io/axllent/mailpit:latest`). Each invocation gets fresh containers and storage. Only HTTP endpoints are published, on random loopback ports; containers, volumes and the network are removed on exit. The replacement backend does not need MariaDB.

## Observe or update the contract

`fixtures/golden/` records the reference responses (Ktor app at `700d5c6`, MariaDB 11.8). Each story captures the ordered steps, statuses and bodies, including bodyless `204`. JSON object formatting is canonicalized; array order, numeric values, and null vs empty vs missing fields are not normalized away. Auth tokens are redacted; nonzero server-generated sync timestamps are normalized. Scenario assertions still run when recording.

`fixtures/observed/` is separate. It records the reference's partial writes after a rejected batch, deletion lost to an equally dated history upload, malformed-request responses, rejection of an expired reset token, and survival of an old session after reset. Do not use this suite to gate a new backend. The expiry story moves the deadline of an actual emailed token inside the labelled disposable reference DB, then tests the API; it does not wait 15 minutes or add a production clock hook.
The observation snapshots mask MariaDB connection IDs, which have no meaning to the client.

Run `record` instead of `verify` only against the reference when intentionally updating snapshots, selecting the suite to record. Inspect the diff. Do not record against a candidate to make a failed comparison pass.

## Candidate acceptance (not reference parity)

On a fresh disposable backend, run `go run . verify --suite history-policy --base-url http://127.0.0.1:8090` from this directory. At equal `updated_at`, a history deletion must survive an active packet in either delivery order and on replay. An explicitly later reading (after both timestamps) must restore the row. Each case reads persisted state via an empty phone's POST. These are proposed replacement semantics, not observations to record as goldens.

On a fresh disposable candidate, run `go run . verify --suite favourites-policy --base-url http://127.0.0.1:8090`. This assertion-only suite checks that newer category and favourite tombstones survive older deletions and stale active copies, and that genuinely later re-adds persist. It also checks partial updates to existing rows and shared tag propagation. It is opt-in and does not record or compare goldens.

To exercise backup and restore, run `python3 restore_drill.py /absolute/path/to/server` on an isolated test host. Use a local executable (or a wrapper that **execs** it) honoring `PORT`, `DATABASE_PATH`, `JWT_SECRET`, `BASE_URL` and `ALLOW_NEW_REGISTER`; Python 3, SQLite support and GnuPG 2.2+ are required. The client uses loopback, but some ports listen on all interfaces. On Linux with unprivileged user namespaces and iproute2, isolate the listeners as well:

```sh
unshare -Urn sh -c 'ip link set lo up && exec python3 restore_drill.py "$1"' sh /absolute/path/to/server
```

The drill uses only temporary directories and synthetic accounts. It takes an online SQLite backup, encrypts it with a disposable passphrase, commits a marker to the live server, then discards the original data. A new process must serve the restored account, history, categories, favourites and deletion markers, **without** the later marker. This proves a local restore from the encrypted artifact; it does not exercise off-host storage, retained keys, backup scheduling or production recovery times.
