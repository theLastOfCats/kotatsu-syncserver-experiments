# Kotatsu HTTP implementations on SQLite and MySQL

`apps/go`, `apps/csharp`, `apps/kotlin`, `apps/axum` (Rust) and `apps/crystal` implement the Kotatsu HTTP interface independently. Each process uses **one** storage backend, selected at startup. The original server is an [external reference](../experiments/README.md#original-ktor-baseline), not part of this repository. Use a separate empty database for each application; the schemas are implementation details, not a migration or interchange format.

## Build and run

Install the tools for the targets you want: Go 1.26+ (also needed for test clients), .NET SDK 11, JDK 21, Rust/Cargo 1.94+, or Crystal/Shards via `mise install crystal@latest`. Run from the repository root:

Crystal links against the system `libargon2` for password hashing. On Debian/Ubuntu, install `libargon2-dev` to build; the executable needs `libargon2-1` at runtime.

Axum uses `jsonwebtoken` 12 with AWS-LC for HS256, which requires a C toolchain and CMake to build. Versions before 10.3.0 are affected by [CVE-2026-25537](https://github.com/advisories/GHSA-h395-gr6q-cpjc).

```sh
./bench/build.sh                     # or pass individual targets: tools go csharp kotlin axum crystal
export PORT=8080 BASE_URL=http://127.0.0.1:8080
export JWT_SECRET="$(openssl rand -hex 32)"   # persist this value across restarts
export DATABASE_DIALECT=sqlite DATABASE_PATH="$PWD/kotatsu.sqlite"
build/bench/go
# Instead run: build/bench/csharp/Kotatsu
#          or: apps/kotlin/build/install/kotatsu-kotlin/bin/kotatsu-kotlin
#          or: build/bench/axum
#          or: build/bench/crystal
```

For MySQL, provision a separate MySQL 8.4/InnoDB database and a user allowed to create tables in it. Instead of the SQLite settings export `DATABASE_DIALECT=mysql`, `DATABASE_HOST`, `DATABASE_PORT`, `DATABASE_NAME`, `DATABASE_USER`, `DATABASE_PASSWORD`, then launch the same executable. Hook up real SMTP with `MAIL_PROVIDER=smtp` and `SMTP_HOST`, `SMTP_PORT`, `SMTP_FROM` plus any required `SMTP_USERNAME`, `SMTP_PASSWORD`; local development can use console mail. `BASE_URL` is the externally visible origin for recovery links. Keep local mail output private. `ALLOW_NEW_REGISTER=false` disables creating accounts by first login.

### Rate limits

All ports accept these positive integer settings:

| Environment variable | Default | Scope |
|---|---:|---|
| `RATE_LIMIT_API_PER_SECOND` | 200 | Shared API budget per instance, including JWT sync and catalog reads |
| `RATE_LIMIT_AUTH_PER_MINUTE` | 5 | Login and registration per peer IP |
| `RATE_LIMIT_FORGOT_PER_10_MINUTES` | 5 | Reset email requests per peer IP |
| `RATE_LIMIT_RESET_PER_10_MINUTES` | 5 | Password reset submissions per peer IP |
| `RATE_LIMIT_PASSWORD_CONCURRENCY` | 2 | Simultaneous password work shared by login and reset |

JWT sync does not hash passwords and has no separate IP or account quota. Health and reset deeplinks are outside the API budget. Exhausting a request quota returns `429` with `Retry-After`; password workers reject excess work with `503` and `Retry-After: 1` rather than queuing. Each Argon2id operation uses 64 MiB of working memory. The shared API ceiling is a configurable backstop, not a measured capacity or network-level DDoS defense.

IP limits use the direct peer address, not forwarded headers. Behind a reverse proxy, enforce per-client limits at the trusted edge and tune the application's IP quotas for the proxy's aggregate traffic.

### Separate native AOT builds (Linux x86-64)

`csharp` and `kotlin` remain JIT targets. `csharp-aot` (.NET NativeAOT) and `kotlin-aot` (GraalVM Native Image) are opt-in builds of the same applications, not alternate servers. Install the native compiler toolchain and keep the JDK 21 used by Gradle separate from GraalVM 25; Gradle 9.0.0 does not support running on JDK 25.

```sh
mise install java@21 java@oracle-graalvm-25.0.5
export JDK21_HOME="$(mise where java@21)"
export GRAALVM_HOME="$(mise where java@oracle-graalvm-25.0.5)"
JAVA_HOME="$JDK21_HOME" ./bench/build.sh csharp-aot kotlin-aot
build/bench/csharp-aot/Kotatsu                # .NET native executable
# or: build/bench/kotlin-aot/kotatsu-kotlin   # GraalVM native executable
```

Keep each publish directory intact, including its native libraries. Neither profile is selected by the default build or benchmark. Pass `--apps csharp-aot kotlin-aot` to `matrix.py` for `verify`, `load` or `memory`, and to `report.py` when reporting only these two profiles.

## Reproduce the HTTP checks and benchmarks

Requires Linux, Python 3, rootless Podman, and these local images. `matrix.py` never pulls, uses existing deployments, or accepts a target server URL. It starts each application in a temporary directory on the checkout filesystem. Every MySQL cell gets a fresh on-disk container volume; verify cells get a private Mailpit. Published fixture ports bind to loopback. Do not put the checkout or container storage on tmpfs if comparing disk writes.

```sh
podman pull docker.io/library/mysql:8.4
podman pull docker.io/axllent/mailpit:latest
./bench/matrix.py verify --output build/bench/verified.jsonl
./bench/matrix.py load --clients 4 --library 50 --warmup 10 --rounds 50 --trials 3 \
  --output build/bench/load.jsonl
./bench/report.py build/bench/load.jsonl
```

Select a subset with `--apps` and `--databases`. Supply new output paths for each invocation (no overwrite). For fixed CPU placement use `--app-cpus`, `--mysql-cpus`, `--client-cpus` with disjoint CPU lists from `lscpu -e`; leave them unset on small machines. `MYSQL_IMAGE` and `MAILPIT_IMAGE` may name locally available immutable digests.

To add results for just Rust and Crystal, pass `--apps axum crystal` to each `matrix.py` invocation. Report their two databases with `./bench/report.py FILE --apps axum crystal`; for the original files alone use `--apps go csharp kotlin`. To compare all five applications, pass both JSONL files from the **same profile** to one `report.py` call without `--apps`.

* **Verify:** run the checked-in sync and password recovery snapshots, the history conflict policy (both delivery orders), and HTTP checks for health, HEAD, `/me`, catalog pagination/details and reset links. Each suite starts a fresh server process against the cell's disposable database, so test suites do not consume one shared login window. It uses the normal rate limits. Never regenerate reference goldens from candidates.
* **Load:** independent accounts, a persistent HTTP client, and a full library per client. Time changed history upload, identical history replay and the two-request favourites exchange separately. Registration, seeding and final database readback via the HTTP API are outside timed phases. A wrong status or incorrect recovered state invalidates the run rather than adding a fast error to the results.
* **Rate limits during measurement:** `load` and `memory` keep all limiters active and set `RATE_LIMIT_API_PER_SECOND` to 1000 unless the caller supplies a different value. The runner records the effective configuration. The five-auth default covers the four reader accounts plus the optional importer.
* **Concurrent import (optional):** `--neighbour-library 500` adds one new account uploading 500 history cards while existing readers update a single page and send their full libraries. Request starts are coordinated on the client; actual overlap inside the database is not assumed. For short imports there may be too few reader completions for a useful p95; the JSON reports the count and omits that percentile below 100 samples.
* **Metrics:** one operation means a completed user operation, not a SQL statement; the favourites exchange includes both HTTP requests. Individual JSONL records contain sample counts, duration, p50, p95 and aggregate throughput. The report uses the median trial RPS and median trial p95, not an average of percentiles. App and MySQL process RSS are sampled every 100 ms during the client invocation **including setup**; they are not container memory or idle RSS. Separate fresh instances run sequentially in deterministic shuffled order.

SQLite uses WAL with FULL synchronous commits; the runner checks MySQL `innodb_flush_log_at_trx_commit=1` and `sync_binlog=1`. The comparison is between entire implementations (runtime, HTTP, serialization, SQL, drivers, connection pools), not an isolated measurement of the languages or ORMs.

## Current port comparison

The [four-client trials](results/current-stacks-memory-four.jsonl) behind the [README tables](../README.md#how-the-stacks-compare) use the refreshed port dependencies at `92ca90d9e9`. Go uses `modernc.org/sqlite` 1.63.0 and MySQL driver 1.10.2; C# uses Microsoft.Data.Sqlite 11.0.0 and MySqlConnector 3.0.0; Kotlin 2.4.21 / Ktor 3.6.1 uses SQLite JDBC 3.54.1.0 and MySQL Connector/J 26.10.0; Axum uses SQLx 0.9.1 with bundled SQLite 3.54.1. Crystal 1.22.3 uses its system SQLite 3.53.4 and the pinned shards.

Build Kotlin on JDK 21 and run both Kotlin JIT variants on the same Oracle GraalVM 25.0.5 used for Native Image. MySQL is 8.4.13; keep the same image digest and CPU placement for comparisons.

```sh
export GRAALVM_HOME="$(mise where java@oracle-graalvm-25.0.5)"
JAVA_HOME="$(mise where java@21)" ./bench/build.sh \
  tools go csharp csharp-aot kotlin-aot kotlin-cio-aot axum crystal
JAVA_HOME="$GRAALVM_HOME" RATE_LIMIT_API_PER_SECOND=1000 ./bench/matrix.py memory \
  --apps go csharp csharp-aot kotlin kotlin-aot kotlin-cio kotlin-cio-aot axum crystal \
  --clients 4 --library 50 --rounds 50 --warmup 10 --trials 3 \
  --app-cpus 0-3 --mysql-cpus 4-5 --client-cpus 6-7 \
  --output build/bench/current-stacks-memory-four.jsonl
```

The fresh snapshot measures complete builds, not a dependency-only A/B against the archival results below. It keeps the shared API limiter enabled at 1,000 requests/s; registration and password hashing run before the measured restart.

## Deployment and CPU scaling

Run the startup and sync profiles sequentially on the builds above. On this Ryzen 7 7800X3D, CPUs 0–7 are on distinct physical cores; `0`, `0-1`, and `0-3` give the server one, two, and four cores. The driver stays on CPUs 6–7 and MySQL on CPUs 4–5. These are CPU affinity tests, not emulated VPS CPU quotas or clock speeds. SQLite shares the app's cores; the MySQL profile is not a one-core deployment with app and database sharing that core.

```sh
export JAVA_HOME="$(mise where java@oracle-graalvm-25.0.5)"
python3 bench/startup.py --app-cpus 0 --client-cpus 6-7 \
  --output build/bench/platform-startup-1core.jsonl
python3 bench/startup.py --app-cpus 0-3 --client-cpus 6-7 \
  --output build/bench/platform-startup-4core.jsonl
RATE_LIMIT_API_PER_SECOND=1000 taskset -c 6-7 python3 bench/scaling.py \
  --app-cpus 0 0-1 0-3 --mysql-cpus 4-5 --client-cpus 6-7 \
  --library 200 --rounds 100 --warmup 25 --trials 3 \
  --output build/bench/platform-scaling.jsonl
```

Startup is process spawn to the first observed `200 Alive`, with 1 ms sleeps between failed probes. Seven paired starts per build and CPU count use a new SQLite file, then the same initialized empty file. Every start uses a new process; filesystem caches are not flushed. The payload is the as-built binary or release directory, including companion files and other-platform assets, excluding `.pdb` and `.dbg`. External shared runtimes and OS libraries are not included. The GraalVM figure is the entire installed Java home, not a minimal runtime image.

Scaling uses the post-restart JWT-only profile, with four clients, 200 history and 200 favourites entries per client, 25 warmup and 100 timed operations per client per phase. The three trials shuffle the build, database and core-count cells. Compare operation throughput within each build and database: speedup is throughput on N cores divided by throughput on one; divide by N for parallel efficiency. Report p99 as the median of per-trial nearest-rank p99 values, not a fleet-wide tail estimate.

CPU is the user plus system time of each process from client resume through final readback, including warmup and any runtime work in that window. Normalized CPU in the summary divides that total by the number of successful timed operations, so it includes amortized warmup and readback. A favourites exchange is two HTTP requests. RSS peaks use 100 ms samples; approximately 1 s samples are kept separately as a time series. No registration, Argon2, or neighbouring import runs in the measured window.

## Measurement environment

Linux x86-64, Ryzen 7 7800X3D, 30 GiB RAM, NVMe/Btrfs. The server process ran on CPUs 0–3, MySQL on 4–5, the HTTP load generator on 6–7 (distinct physical cores). MySQL 8.4.13 ran in rootless Podman via a loopback TCP port; SQLite ran in-process. The database cost is **additional** to the application cost for MySQL. The load generator includes client-side encoding/decoding and verification of the favourites GET in exchange latency.

Go 1.27.4 (`modernc.org/sqlite`); .NET 11 Release, JIT, not NativeAOT (`Microsoft.Data.Sqlite`); Kotlin 2.2.20/Ktor 3.3.0 on JDK 21 (`sqlite-jdbc`). Go uses per-row SQL, .NET batches upserts, and Kotlin loads existing manga per account to decide which cards changed. Differences in round trips therefore matter, especially over MySQL. Account hashing is outside timed operations but can affect the reported RSS. A fixed short warmup is not a claim about long-running JIT steady state.

Three implementation differences limit what the numbers establish:

* **Memory:** In the historical measurements below, Go and Kotlin used Argon2id with 64 MiB per hash, while C# used PBKDF2-SHA256. The `load` profile's peak RSS includes account creation; the post-restart `memory` profile does not.
* **SQLite contention:** Go and Kotlin serialize database work on one connection (10 s busy timeout); C# uses the driver's pooled connections (30 s busy timeout). These are different queueing policies under concurrent writes.
* **MySQL transport and pools:** Go does not request TLS and allows up to 32 connections. Kotlin prefers TLS and allows 16; C# prefers TLS and uses MySqlConnector's default pool limit. Actual negotiated ciphers and pool occupancy were not recorded. These data cannot isolate the cost of a runtime, a driver or TLS.

The two additional native releases use Rust 1.100.0 with Axum 0.8.9 and SQLx 0.8.6, and Crystal 1.22.3 with `HTTP::Server`, crystal-db 0.15.0, crystal-sqlite3 0.24.0 and crystal-mysql 0.18.0. Both serialize SQLite access on one connection. MySQL pool limits are 16 for Axum and 8 for Crystal. Crystal extends the pinned driver's handshake for `caching_sha2_password`: it requires TLS on TCP, permits the benchmark's self-signed certificate only on loopback, and verifies server identity for remote hosts. MySQL readiness probes use a separate admin account so the application's first connection exercises cold authentication. In those measurements Axum used Argon2id with its library defaults and Crystal used bcrypt; the restarted memory profile excludes both.

## Original Go, C# and Kotlin measurements

[HTTP verification](results/verified.jsonl) passed for every pairing. Each of the two load profiles ran three times per pairing (36 runs). Each client held 50 history entries and 50 favourites in five categories, did 10 warmup operations per phase, then 50 measured operations. The four-client profile additionally imported 500 new history entries from a fifth account. Reproduce with the command above, specifying the CPU lists, `--clients 1` or `--clients 4 --neighbour-library 500`.

| App | DB | Changed history, 1 client: p95 ms | Changed history, 4 clients: p95 ms | Changed history, 4 clients: ops/s | Replay, 4 clients: ops/s | Favourites exchange, 4 clients: ops/s |
|---|---|---:|---:|---:|---:|---:|
| Go | SQLite | 7.7 | 42.0 | 145.3 | 143.3 | 126.9 |
| C# | SQLite | 9.1 | 62.7 | 105.3 | 107.4 | 112.1 |
| Kotlin | SQLite | 8.1 | 34.0 | 167.4 | 173.9 | 156.2 |
| Go | MySQL | 30.8 | 45.6 | 104.8 | 108.7 | 87.2 |
| C# | MySQL | 19.4 | 26.7 | 189.9 | 152.3 | 176.0 |
| Kotlin | MySQL | 19.9 | 30.0 | 186.1 | 214.3 | 170.0 |

Raw results: [one client](results/one.jsonl), [four clients + importer](results/four.jsonl). `bench/report.py` prints p95, throughput and separate process RSS peaks. The raw records also include p50 and reader sample counts during the import. Most imports were too short to estimate a meaningful reader p95; a request still waiting for the writer when the import finishes is excluded from that window. Do not use it to claim absence of stalls. These runs do not establish capacity for 100,000 accounts.

## Memory of an already authenticated sync

The `memory` profile separates password hashing from the service being measured. A disposable first process registers the accounts and seeds their data. Once the client verifies persisted state, it pauses with its JWTs and local libraries in memory. The runner stops that server and starts the same executable on the same database and port with the same signing key. The client resumes sync **without signing in again**. For MySQL, `mysqld` remains running with the seeded database.

```sh
./bench/build.sh tools
for n in 1 4; do
  ./bench/matrix.py memory --clients "$n" --library 50 --warmup 10 --rounds 50 --trials 3 \
    --app-cpus 0-3 --mysql-cpus 4-5 --client-cpus 6-7 --output "build/bench/rss-$n.jsonl"
  ./bench/report.py "build/bench/rss-$n.jsonl"
done
```

Per process, the runner reads Linux `VmRSS` three ways: **idle** (3 seconds after the restarted server becomes healthy), **sync peak** (largest sample at 100 ms intervals during warmup, sync and client readback), and **after sync** (10 seconds after the load client finishes). It does not force garbage collection. These are resident pages, not heap allocations, container memory or a recommended memory limit; short peaks between samples can be missed. SQLite lives inside the application process. MySQL has its own RSS column. The neighbour import is excluded because it would register an account after the restart.

Median of three independent trials per cell (MiB). Idle and after-sync columns below come from the four-client profile. Raw records for [one client](results/memory-one.jsonl) and [four clients](results/memory-four.jsonl) include all three phases for both processes.

| App | DB | App idle | App sync peak, 1 client | App sync peak, 4 clients | App after sync, 4 clients | Additional mysqld sync peak, 4 clients |
|---|---|---:|---:|---:|---:|---:|
| Go | SQLite | 16.5 | 24.0 | 27.8 | 26.1 | — |
| C# | SQLite | 80.4 | 97.3 | 122.1 | 121.3 | — |
| Kotlin | SQLite | 123.8 | 208.9 | 238.3 | 231.4 | — |
| Go | MySQL | 14.8 | 22.3 | 25.4 | 22.6 | 472.3 |
| C# | MySQL | 101.1 | 124.2 | 143.3 | 142.1 | 476.9 |
| Kotlin | MySQL | 158.3 | 216.3 | 378.2 | 369.4 | 470.4 |

## Axum and Crystal measurements

[HTTP verification](results/extended-verified.jsonl) passed for Axum and Crystal on both databases: 71 sync steps, 22 history conflict steps, eight recovery steps with Mailpit, and checks of the public HTTP routes for each pairing. The extended profiles retain the original CPU placement, library sizes, warmup, measured rounds and trial count. The original three servers were not rerun.

```bash
./bench/build.sh axum
./bench/build.sh crystal
./bench/build.sh tools
args=(--apps axum crystal --library 50 --warmup 10 --rounds 50 --trials 3 \
      --app-cpus 0-3 --mysql-cpus 4-5 --client-cpus 6-7)
./bench/matrix.py verify --apps axum crystal --output build/bench/check-both.jsonl
./bench/matrix.py load "${args[@]}" --clients 4 --neighbour-library 500 --output build/bench/load-four.jsonl
./bench/matrix.py load "${args[@]}" --clients 1 --output build/bench/load-one.jsonl
for n in 4 1; do
  ./bench/matrix.py memory "${args[@]}" --clients "$n" --output "build/bench/memory-$n.jsonl"
done
```

Median of three trials, from the [one-client](results/extended-one.jsonl) and [four-client](results/extended-four.jsonl) load records:

| App | DB | Changed history, 1 client: p95 ms | Changed history, 4 clients: p95 ms | Changed history, 4 clients: ops/s | Replay, 4 clients: ops/s | Favourites exchange, 4 clients: ops/s |
|---|---|---:|---:|---:|---:|---:|
| Axum | SQLite | 5.9 | 22.8 | 178.6 | 179.6 | 162.6 |
| Crystal | SQLite | 6.3 | 24.9 | 165.4 | 165.5 | 69.1 |
| Axum | MySQL | 17.0 | 22.1 | 235.2 | 219.8 | 232.6 |
| Crystal | MySQL | 22.5 | 40.3 | 120.1 | 122.0 | 58.7 |

Post-restart RSS in MiB, from the [one-client](results/extended-memory-one.jsonl) and [four-client](results/extended-memory-four.jsonl) memory profiles:

| App | DB | App idle | App sync peak, 1 client | App sync peak, 4 clients | App after sync, 4 clients | Additional mysqld sync peak, 4 clients |
|---|---|---:|---:|---:|---:|---:|
| Axum | SQLite | 8.2 | 10.1 | 11.5 | 11.3 | — |
| Crystal | SQLite | 9.6 | 14.5 | 15.0 | 14.8 | — |
| Axum | MySQL | 8.1 | 9.4 | 11.7 | 11.6 | 484.8 |
| Crystal | MySQL | 14.0 | 17.2 | 21.1 | 20.9 | 475.1 |

On SQLite with four clients the two services were close on changed history, but Axum completed more than twice as many favourites exchanges per second. MySQL raised Axum's four-client throughput but not Crystal's; for one client both had lower history latency on SQLite. The MySQL process alone used substantially more resident memory than either native server.

### Axum A/B: reuse the ordered sync snapshot

Both binaries include the same deletion-conflict fix (`b03d7e710e5`); this control still rereads the whole package after writing. The candidate (`689bf15de58a`) patches the already loaded snapshot for updates to existing keys. New keys or changed shared manga/tag data still use database readback. Both variants passed [live HTTP verification](results/axum-reuse-verified.jsonl) on SQLite and MySQL, with `favourites-policy` covering newer tombstones, a later re-add, partial updates with ordered readback, and cross-account tag propagation.

For an existing-row HTTP POST with two tagged cards, MySQL 8.4.13's [`Com_select` count](results/axum-reuse-sql.jsonl) dropped from **7 to 5** for history and **8 to 6** for favourites. Both variants returned bodyless `204` and read back the changed data and tags. The probe counts between consecutive `SHOW GLOBAL STATUS LIKE 'Com_select'` calls; seeding and the readback GET are outside that interval.

Load profiles use the same compiler, client and CPU placement as this section. Each variant runs on a fresh database, three trials per pairing, with 50 entries and 10 warmup plus 50 timed operations per phase. The four-client profile also includes the 500-entry import. The JSONL records carry the executable SHA-256 to distinguish both builds. No memory-after-restart measurements were taken for this A/B.

Medians, control → candidate. History replay does not execute the removed readback and is included as a comparison for run-to-run variation. Raw profiles: [one client](results/axum-reuse-one.jsonl), [four clients](results/axum-reuse-four.jsonl).

| DB | Clients | Changed history, ops/s | Changed history p95, ms | Replay, ops/s | Favourites exchange, ops/s | Favourites exchange p95, ms |
|---|---:|---:|---:|---:|---:|---:|
| SQLite | 1 | 174.3 → 176.6 | 5.9 → 5.8 | 178.7 → 178.6 | 155.1 → 156.7 | 6.8 → 6.6 |
| SQLite | 4 | 177.7 → 183.4 | 22.9 → 22.1 | 181.2 → 179.1 | 162.8 → 165.9 | 25.0 → 25.8 |
| MySQL | 1 | 67.0 → 81.1 | 16.5 → 16.3 | 79.1 → 81.4 | 61.6 → 63.4 | 17.1 → 17.1 |
| MySQL | 4 | 236.0 → 236.7 | 21.7 → 21.5 | 221.9 → 218.0 | 229.0 → 234.0 | 21.9 → 22.5 |

Single-client changed-history throughput on MySQL was higher in all three candidate trials, but varied from 70.0 to 81.9 ops/s (control: 66.4–67.8). The four-client MySQL history median barely changed. SQLite changed-history throughput increased 3.2% at four clients; favourites increased about 2% on both databases while its p95 rose slightly. The SQL reduction is direct evidence; these short HTTP profiles do not establish a uniform speedup.

## Original Ktor reference on MariaDB

The `reference` row is the **original service**, not `apps/kotlin`. It uses Ktor with Netty. The historical JAR extracted from `localhost/kotatsu-parity-reference:700d5c6` has SHA-256 `0c73622c015798bf3779c6dcbd65c930f851c7aac5e0f294c84faa90ef0fa0e1`. The stored figures were collected with the historical runner and exact artifact. Reproduce them from their historical revision. The current runner uses the API ceiling configured in a current original server and records `RATE_LIMIT_API_PER_SECOND` (default 200). No replacement code or database schema is used.

Obtain the original reference JAR separately as described in the [reference setup](../experiments/README.md#original-ktor-baseline).

```bash
# From the repository root, with JDK 21 selected in JAVA_HOME.
./bench/build.sh tools
jar=/absolute/path/to/current-reference.jar
for n in 4 1; do
  for mode in load memory; do
    python3 bench/reference.py "$mode" --jar "$jar" --clients "$n" --trials 3 \
      --output "build/bench/original-$mode-$n.jsonl"
    python3 bench/report.py "build/bench/original-$mode-$n.jsonl" --apps reference
  done
done
```

Every trial starts a fresh rootless `mariadb:11.8` container on disk; no existing database is used. The post-restart profile measures the Java and `mariadbd` processes separately. Compare **stacks**, not just application code: the replacements use SQLite or MySQL 8.4, not MariaDB. To compare all eleven rows in one report, pass the three JSONL files for the same profile followed by `--apps reference axum crystal go csharp kotlin`.

Measured MariaDB version: 11.8.9 (InnoDB, `innodb_flush_log_at_trx_commit=1`, binary logging **off**, `sync_binlog=0`). The MySQL 8.4.13 runs used `sync_binlog=1`; the SQL engines and their logging costs are not interchangeable.

Median of three trials: throughput and changed-history p95 come from the load profile; per-process RSS peaks come from authenticated sync **after restart**, not from account creation.

| Clients | Changed history, ops/s | Replay, ops/s | Favourites GET+POST, ops/s | Changed history p95, ms | App sync peak, MiB | MariaDB sync peak, MiB |
|---|---:|---:|---:|---:|---:|---:|
| 1 | 11.1 | 12.0 | 9.1 | 100.0 | 420.9 | 152.0 |
| 4 | 33.5 | 35.0 | 26.1 | 129.1 | 421.2 | 154.0 |

The four-client [load](results/reference-load-four.jsonl) and [memory](results/reference-memory-four.jsonl) records contain separate distributions. In the additional import scenario the original stored all 500 rows; the median import duration was 7.91 seconds while four other clients were syncing.

The one-client [load](results/reference-load-one.jsonl) and [memory](results/reference-memory-one.jsonl) records use the same library and phase sizes without the neighbour import.

### Same reference JAR on GraalVM 25.0.5

These historical timings use the same Netty reference JAR, client and MariaDB image. Run this recipe from the revision that recorded them. The first run used Temurin 21.0.12.1; the second uses Oracle GraalVM 25.0.5+9.1. This compares two JDK distributions and versions, not the JDK version in isolation.

```bash
export JAVA_HOME="$(mise where java@oracle-graalvm-25.0.5)"
jar=/absolute/path/to/exact-reference.jar # Do not run Gradle on JDK 25.
for n in 4 1; do
  for mode in load memory; do
    python3 bench/reference.py "$mode" --jar "$jar" --clients "$n" --trials 3 \
      --output "build/bench/original-graal25-$mode-$n.jsonl"
    python3 bench/report.py "build/bench/original-graal25-$mode-$n.jsonl" --apps reference
  done
done
```

Median of three trials per profile, same reference bytecode, MariaDB 11.8.9 image, CPU placement and workload:

| JIT runtime | Clients | Changed history, ops/s | Changed history p95, ms | Replay, ops/s | Favourites exchanges/s | App idle, MiB | App sync peak, MiB | DB sync peak, MiB |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| Temurin 21.0.12.1 | 1 | 11.1 | 100.0 | 12.0 | 9.1 | 231.4 | 420.9 | 152.0 |
| GraalVM 25.0.5 | 1 | 11.3 | 98.1 | 12.9 | 9.5 | 215.5 | 458.6 | 152.3 |
| Temurin 21.0.12.1 | 4 | 33.5 | 129.1 | 35.0 | 26.1 | 225.3 | 421.2 | 154.0 |
| GraalVM 25.0.5 | 4 | 33.4 | 130.6 | 35.6 | 27.1 | 215.4 | 479.4 | 153.9 |

GraalVM 25 did not materially change changed-history throughput; the four-client favourites median rose from 26.1 to 27.1 exchanges/s. The sample peak RSS after restart grew 9% with one client and 14% with four, despite lower idle RSS. The four-client 500-entry import took 7.7 s versus 7.9 s on Temurin 21. Treat these as workload measurements, not general JVM performance claims.

GraalVM 25 raw records: one client ([load](results/reference-graal25-load-one.jsonl), [RSS](results/reference-graal25-memory-one.jsonl)); four clients ([load](results/reference-graal25-load-four.jsonl), [RSS](results/reference-graal25-memory-four.jsonl)). Select one JDK's reference file per `report.py` invocation; both have the same `reference` app key.

### Original Netty server as a Native Image

The original `700d5c6` JAR is the build input; this is not the replacement Kotlin server. Use the exact JAR from the reference image, not a rebuilt JAR with a different hash. The version-pinned build uses Oracle GraalVM 25.0.5+9.1 and `--no-fallback`. Keep the companion libraries next to the executable. [Ktor officially supports CIO for Native Image](https://ktor.io/docs/graalvm.html); this Netty build is an experiment, not a supported deployment.

Two adaptations are compiled into the image only. A Flyway plugin finds the exact JAR's embedded SQL via a build-time index; Flyway still applies the migrations and manages their checksums. A Ktorm substitution identifies generated JDK proxies as non-value classes, avoiding a Kotlin reflection lookup that Native Image does not support. AOT metadata also registers the `UserInfo` serializer for `/me`. The reference JAR and the JIT runs are unchanged.

```bash
# Run the historical recipe from the revision that recorded the archived timings.
./bench/build.sh tools
GRAALVM_HOME="$(mise where java@oracle-graalvm-25.0.5)" \
  ./bench/reference-native.sh /path/to/exact-reference.jar
native=build/bench/reference-aot/kotatsu-syncserver
python3 bench/reference.py verify --native "$native" --output build/bench/reference-native-check.jsonl
for n in 4 1; do
  for mode in load memory; do
    output="build/bench/original-native-$mode-$n.jsonl"
    python3 bench/reference.py "$mode" --native "$native" --clients "$n" --trials 3 --output "$output"
    python3 bench/report.py "$output" --apps reference
  done
done
```

[HTTP verification](results/reference-aot-verified.jsonl) passed 71 sync and 8 recovery steps. The verify command uses separate fresh MariaDB databases for sync and authenticated SMTP recovery. Rate limits remained active during verification. The archived timings require the historical runner; the current runner does not override the archived binary's built-in cap.

Medians of three trials per profile, with the same Netty server, MariaDB 11.8.9 image, CPU placement and workload. RSS peaks are sampled during authenticated sync after restart.

| GraalVM 25.0.5 mode | Clients | Changed history, ops/s | Changed history p95, ms | Replay, ops/s | Favourites exchanges/s | App sync peak, MiB | MariaDB sync peak, MiB |
|---|---:|---:|---:|---:|---:|---:|---:|
| JIT | 1 | 11.3 | 98.1 | 12.9 | 9.5 | 458.6 | 152.3 |
| Native Image | 1 | 10.7 | 99.5 | 10.8 | 8.2 | 130.5 | 151.4 |
| JIT | 4 | 33.4 | 130.6 | 35.6 | 27.1 | 479.4 | 153.9 |
| Native Image | 4 | 30.7 | 142.7 | 31.1 | 24.1 | 132.5 | 153.6 |

Native Image used 72% less app RSS after restart. With four clients it processed 8% less changed history and 11% fewer favourites exchanges per second. The 500-entry import took 7.8 s versus 7.7 s on JIT. MariaDB's separate memory stayed roughly constant.

Native Image raw records: one client ([load](results/reference-aot-load-one.jsonl), [RSS](results/reference-aot-memory-one.jsonl)); four clients ([load](results/reference-aot-load-four.jsonl), [RSS](results/reference-aot-memory-four.jsonl)). The same executable SHA-256 is recorded in both acceptance results and all 12 measurements.

## AOT versus JIT

Run the same HTTP workloads against both builds. The four-client load profile includes a neighbouring importer; neither post-restart RSS profile does. Keep builds and other workloads off the benchmark cores while this loop runs.

[HTTP verification](results/aot-verified.jsonl) passed for all eight build/database pairs (sync, equal-timestamp history conflicts, SMTP recovery and the public routes). Kotlin uses the same Ktor application and dependencies in both modes: the regular JVM launcher runs on Oracle GraalVM 25.0.5+9.1 with its default G1, and `native-image --no-fallback` produces an ELF with its default Serial GC and system libc/zlib. The earlier JDK 21 figures are not reused here. C# uses .NET 11.0.0 in both modes; NativeAOT bundles the .NET runtime code in the ELF and keeps the SQLite shared library in its publish directory. The JSON serialization metadata is shared across the C# builds.

The [official Ktor GraalVM guide](https://ktor.io/docs/graalvm.html) supports CIO only. This benchmark deliberately keeps Netty 4.2.4.Final in both Ktor 3.3.0 builds so the comparison does not also change the HTTP engine. The Netty Native Image is an experimental, unsupported configuration, not a production recommendation. It initializes Netty at runtime and enables shared arenas for Java 25 direct-buffer cleanup. Without shared-arena support, a parallel post-restart SQLite run hit an HTTP 500 even though the short contract passed. Native measurements use the rebuilt executable; the recorded SHA-256 identifies it.

```bash
JAVA_HOME="$JDK21_HOME" ./bench/build.sh tools csharp kotlin csharp-aot kotlin-aot
# Hold the JVM generation and distribution fixed across Kotlin JIT and Native Image.
export JAVA_HOME="$GRAALVM_HOME"
apps=(csharp csharp-aot kotlin kotlin-aot)
./bench/matrix.py verify --apps "${apps[@]}" --output build/bench/aot-check.jsonl
args=(--apps "${apps[@]}" --library 50 --warmup 10 --rounds 50 --trials 3 \
      --app-cpus 0-3 --mysql-cpus 4-5 --client-cpus 6-7)
for n in 4 1; do
  extra=()
  if [[ $n == 4 ]]; then extra=(--neighbour-library 500); fi
  ./bench/matrix.py load "${args[@]}" --clients "$n" "${extra[@]}" --output "build/bench/aot-load-$n.jsonl"
  ./bench/matrix.py memory "${args[@]}" --clients "$n" --output "build/bench/aot-memory-$n.jsonl"
  ./bench/report.py "build/bench/aot-load-$n.jsonl" --apps "${apps[@]}"
  ./bench/report.py "build/bench/aot-memory-$n.jsonl" --apps "${apps[@]}"
done
```

Four-client [load results](results/aot-four.jsonl), medians of three trials. The favourites column counts complete GET+POST exchanges.

| Build | DB | Changed history, ops/s | Changed history p95, ms | Replay, ops/s | Favourites exchanges/s |
|---|---|---:|---:|---:|---:|
| C# JIT | SQLite | 104.5 | 16.9 | 109.2 | 112.6 |
| C# NativeAOT | SQLite | 124.5 | 8.1 | 125.2 | 104.9 |
| Kotlin JIT 25 | SQLite | 169.9 | 29.5 | 174.1 | 158.7 |
| Kotlin Native Image 25 | SQLite | 171.5 | 29.7 | 172.3 | 161.9 |
| C# JIT | MySQL | 179.9 | 29.8 | 140.1 | 152.6 |
| C# NativeAOT | MySQL | 218.0 | 24.9 | 218.7 | 202.3 |
| Kotlin JIT 25 | MySQL | 201.6 | 25.8 | 234.4 | 176.4 |
| Kotlin Native Image 25 | MySQL | 221.9 | 25.0 | 228.2 | 153.8 |

Four-client [authenticated sync RSS](results/aot-memory-four.jsonl) after restart (MiB, medians of three trials):

| Build | DB | App idle | App sync peak | App after sync | DB sync peak |
|---|---|---:|---:|---:|---:|
| C# JIT | SQLite | 80.9 | 119.4 | 119.3 | — |
| C# NativeAOT | SQLite | 40.5 | 57.8 | 54.8 | — |
| Kotlin JIT 25 | SQLite | 152.2 | 280.7 | 236.7 | — |
| Kotlin Native Image 25 | SQLite | 62.5 | 116.2 | 112.1 | — |
| C# JIT | MySQL | 101.5 | 140.8 | 138.9 | 475.2 |
| C# NativeAOT | MySQL | 51.6 | 93.1 | 85.7 | 477.1 |
| Kotlin JIT 25 | MySQL | 184.2 | 431.0 | 387.1 | 470.3 |
| Kotlin Native Image 25 | MySQL | 66.9 | 121.5 | 114.7 | 470.4 |

The [one-client load](results/aot-one.jsonl) and [one-client restart RSS](results/aot-memory-one.jsonl) use the same library sizes and timed phases without the neighbour import. On SQLite, Netty AOT roughly matched Kotlin JIT throughput with four clients while cutting app RSS by more than half. On MySQL it gained about 10% for changed history but lost about 13% for favourites. C# NativeAOT gained about 21% and 33% respectively on MySQL; on SQLite it traded lower memory and higher history throughput for lower median favourites throughput.

## Kotlin CIO versus Netty

CIO uses the same Ktor application, database drivers and HTTP workloads as Netty, with a separate build for each execution mode. CIO is the officially supported Ktor engine for GraalVM Native Image. Select these opt-in profiles to keep the Netty results and compare the engine within each mode:

```bash
JAVA_HOME="$JDK21_HOME" ./bench/build.sh kotlin-cio kotlin-cio-aot
export JAVA_HOME="$GRAALVM_HOME"
apps=(kotlin-cio kotlin-cio-aot)
./bench/matrix.py verify --apps "${apps[@]}" --output build/bench/cio-check.jsonl
args=(--apps "${apps[@]}" --library 50 --warmup 10 --rounds 50 --trials 3 \
      --app-cpus 0-3 --mysql-cpus 4-5 --client-cpus 6-7)
for n in 4 1; do
  extra=()
  if [[ $n == 4 ]]; then extra=(--neighbour-library 500); fi
  ./bench/matrix.py load "${args[@]}" --clients "$n" "${extra[@]}" --output "build/bench/cio-load-$n.jsonl"
  ./bench/matrix.py memory "${args[@]}" --clients "$n" --output "build/bench/cio-memory-$n.jsonl"
done
```

[HTTP verification](results/cio-verified.jsonl) passed for CIO JIT and Native Image on both databases, including real SMTP recovery. Both use Oracle GraalVM 25.0.5; the CIO Native Image uses Serial GC, as does the Netty image. The CIO runtime does not have Netty on its classpath and its native build does not need the Netty initialization or shared-arena flags.

Four-client medians from the [load](results/cio-four.jsonl) and [restart RSS](results/cio-memory-four.jsonl) records. The matching Netty results are in the preceding section.

| Build | DB | Changed history, ops/s | Changed history p95, ms | Replay, ops/s | Favourites exchanges/s | App sync peak, MiB | DB sync peak, MiB |
|---|---|---:|---:|---:|---:|---:|---:|
| Kotlin CIO JIT 25 | SQLite | 163.4 | 31.0 | 173.0 | 157.6 | 272.2 | — |
| Kotlin CIO Native Image 25 | SQLite | 171.8 | 29.4 | 166.6 | 157.8 | 105.5 | — |
| Kotlin CIO JIT 25 | MySQL | 195.7 | 25.6 | 227.7 | 178.4 | 417.4 | 470.4 |
| Kotlin CIO Native Image 25 | MySQL | 214.9 | 24.7 | 226.3 | 174.2 | 109.9 | 470.4 |

The CIO native build cut sampled app RSS by 61% on SQLite and 74% on MySQL versus CIO JIT. At four clients CIO AOT and Netty AOT had almost identical history throughput on SQLite. On MySQL, CIO AOT handled more favourites exchanges (174.2 vs 153.8/s), while Netty AOT was ahead on changed history (221.9 vs 214.9/s). These are medians of three trials; small differences are not general speed claims.

With one client, CIO AOT on SQLite reduced changed-history p95 from 8.3 to 6.9 ms and app sync peak from 249.9 to 106.1 MiB; on MySQL the corresponding changes were 21.0 to 19.9 ms and 255.9 to 109.1 MiB. See the [one-client load](results/cio-one.jsonl) and [one-client restart RSS](results/cio-memory-one.jsonl) for all phases and trials.

For a single combined comparison (C#, Kotlin Netty and Kotlin CIO in both runtime modes), pass `aot-PROFILE.jsonl` and `cio-PROFILE.jsonl` for the same profile to `bench/report.py --apps csharp csharp-aot kotlin kotlin-aot kotlin-cio kotlin-cio-aot`.

## Sync hot-path A/B: Go, Crystal, C#, Kotlin and original Ktor

Each candidate is compared with its own previous build. These are not comparisons between languages or database engines. The changes under test:

| Implementation | Change |
|---|---|
| Go and Crystal | Fetch only the request's history clocks, category clocks/existence, and favourite clocks in account-scoped batches. For 50 distinct rows in one category, these reads fall from 50 to 1 for history and 101 to 2 for favourites. Keep row-write order and update the local clocks after each accepted write. |
| C# JIT and NativeAOT | Skip history UPSERTs for accepted rows whose persisted fields are bit-for-bit unchanged. Still write catalogue data, read back the package, and set the sync timestamp. |
| Kotlin Netty/CIO, JIT and Native Image | Reuse the decoded manga card from the resource snapshot, including across favourite categories. Look up only distinct incoming IDs absent from that snapshot, up to 900 per query. With all IDs present, merge SELECTs fall from 5 to 4 for history and 7 to 6 for favourites. |
| Original Ktor, GraalVM 25 JIT | Evaluate `MangaEntity.tags` once per DTO instead of twice. The getter queries Ktorm; both reads used to happen even for an empty tag set. |

[Live HTTP verification](results/remaining-ab-verified.jsonl) passed on SQLite and MySQL for all eight replacement builds and on MariaDB for the original Ktor candidate. Additional live probes covered 501-row batches, duplicate merges and missing-category rollback in Go and Crystal; signed-zero response parity against both C# controls; and 901 cross-resource IDs, multiple favourite categories and account-scoped tags in Kotlin.

The original service source and its historical JAR remain unchanged. The separate experiment compiles just `ModelMapping.kt` with Kotlin 2.2.20 targeting JVM 21 and replaces only `org/kotatsu/model/manga/ModelMappingKt.class` in a copy of the historical JAR. The source delta is the `MangaEntity.toManga()` argument:

```kotlin
tags = tags.let { tags -> tags.mapTo(HashSet(tags.size)) { it.toTag() } },
```

The archive keeps the original entry order, duplicate resource names and all other entry contents. The verification runs with the global rate limit enabled. Timed original Ktor runs use the existing load-only bypass on both sides, so those numbers are not production request capacity. This experiment does not rebuild the original Netty Native Image.

Same four-client profile as above: fresh databases, 50 entries per client, 10 warmup and 50 timed operations per phase, and a separate 500-entry neighbour import. App, database and client use CPUs `0-3`, `4-5` and `6-7`, respectively. Three trials per variant and database (102 in total), sequentially on the same host; the controls were rerun, not copied from earlier results. Kotlin JIT and Native Image use GraalVM 25.0.5; the timed client and database durability settings are unchanged. No new post-restart memory measurements.

Raw trials: [Go/Crystal](results/remaining-ab-native-four.jsonl), [C#](results/remaining-ab-csharp-four.jsonl), [Kotlin](results/remaining-ab-kotlin-four.jsonl), [original Ktor](results/remaining-ab-reference-four.jsonl). Replacement controls have the `-control` suffix; the original uses a separate `variant` field. Artifact SHA-256 distinguishes the control and candidate in every trial.

Medians, control → candidate. Each cell shows throughput (operations/s), then p95 (ms). One favourites operation is a GET+POST exchange.

| Build | DB | Changed history | History replay | Favourites exchange |
|---|---|---:|---:|---:|
| Go | SQLite | 147.0 → 152.8 · 41.3 → 39.6 | 146.3 → 150.4 · 41.1 → 40.1 | 129.8 → 138.8 · 44.3 → 42.0 |
| Go | MySQL | 106.1 → 121.5 · 49.7 → 40.6 | 109.1 → 119.3 · 45.2 → 41.6 | 88.5 → 113.5 · 54.7 → 44.6 |
| Crystal | SQLite | 165.8 → 165.8 · 24.7 → 24.6 | 165.6 → 165.6 · 24.7 → 24.6 | 68.9 → 69.3 · 65.1 → 66.1 |
| Crystal | MySQL | 120.2 → 184.4 · 44.7 → 25.8 | 123.9 → 179.1 · 35.4 → 29.8 | 59.5 → 62.9 · 74.7 → 70.2 |
| C# JIT | SQLite | 104.4 → 118.2 · 62.5 → 61.8 | 110.0 → 118.7 · 9.4 → 8.8 | 120.9 → 113.6 · 9.5 → 9.1 |
| C# JIT | MySQL | 177.2 → 200.0 · 29.9 → 25.2 | 172.2 → 177.8 · 34.3 → 29.1 | 153.1 → 164.8 · 35.1 → 34.8 |
| C# NativeAOT | SQLite | 122.9 → 127.5 · 7.9 → 7.5 | 125.1 → 136.1 · 7.9 → 7.4 | 124.1 → 121.7 · 8.3 → 13.3 |
| C# NativeAOT | MySQL | 225.0 → 229.8 · 22.7 → 22.4 | 223.3 → 214.7 · 20.7 → 22.8 | 217.4 → 224.1 · 25.2 → 24.9 |
| Kotlin Netty JIT | SQLite | 168.4 → 170.4 · 29.4 → 30.2 | 170.4 → 174.8 · 26.9 → 28.3 | 159.4 → 161.3 · 36.3 → 36.7 |
| Kotlin Netty JIT | MySQL | 199.2 → 201.4 · 25.7 → 25.6 | 225.5 → 218.0 · 23.5 → 24.8 | 174.3 → 182.9 · 30.1 → 29.6 |
| Kotlin Netty AOT | SQLite | 170.2 → 172.4 · 29.7 → 29.0 | 170.3 → 174.3 · 30.1 → 29.4 | 160.7 → 165.3 · 31.2 → 31.0 |
| Kotlin Netty AOT | MySQL | 228.5 → 223.7 · 22.4 → 23.7 | 225.7 → 226.5 · 23.5 → 22.9 | 182.6 → 171.0 · 26.6 → 32.7 |
| Kotlin CIO JIT | SQLite | 167.2 → 167.8 · 30.6 → 29.9 | 175.0 → 175.5 · 24.6 → 27.3 | 159.3 → 161.7 · 36.1 → 35.5 |
| Kotlin CIO JIT | MySQL | 210.6 → 221.8 · 25.2 → 21.9 | 220.3 → 231.5 · 23.8 → 20.6 | 177.5 → 179.6 · 30.8 → 29.7 |
| Kotlin CIO AOT | SQLite | 169.4 → 171.6 · 29.5 → 29.1 | 169.9 → 173.4 · 29.4 → 28.9 | 161.9 → 164.7 · 30.8 → 30.3 |
| Kotlin CIO AOT | MySQL | 232.2 → 230.8 · 22.5 → 22.2 | 229.6 → 231.2 · 22.3 → 22.7 | 184.2 → 172.3 · 25.4 → 31.9 |
| Original Ktor Netty JIT | MariaDB | 35.7 → 54.3 · 119.9 → 81.7 | 37.2 → 58.9 · 113.3 → 75.5 | 28.5 → 45.2 · 150.4 → 96.2 |

The Kotlin candidate (`39e1102e60`) was reverted. Fewer SELECTs did not translate to a uniform improvement: the candidate gave modest gains in several profiles, but MySQL favourites throughput fell 6–7% and p95 rose 23–25% in both Native Images. The measurements establish the regression in this workload, not its cause.

Go and Crystal gained most on MySQL, where every candidate trial completed history and favourites faster than its corresponding control. Crystal was essentially unchanged on SQLite. C# replay improved consistently on SQLite; MySQL did not see a consistent improvement across runtime modes. C# favourites did not change in code and still moved in both directions, a useful check on variation in these short trials.

Original Ktor improved in all three trials on each operation. Reading tags once raised median throughput by 52–59% across the measured phases while lowering p95 by 32–36%. This is a comparison of two original Ktor JARs on the same MariaDB stack, not a result for the replacement Kotlin port.

## One Go backend on MySQL, MariaDB and PostgreSQL

This is a separate standalone-database experiment, not a change to the default builds. The [opt-in patch and reproduction instructions](../experiments/README.md#one-go-binary-on-mysql-mariadb-and-postgresql) include the PostgreSQL adapter and benchmark runner used for these results. MySQL and MariaDB share the same driver and SQL; PostgreSQL uses pgx with equivalent tables, merge logic and transaction boundaries. Each driver sends parameterized statements in one round trip without a prepared-statement cache (client interpolation for the MySQL driver, extended protocol for pgx). The pool is capped at 32 open and 8 idle connections on all three.

Official images: MySQL 9.7.4, MariaDB 12.3.3 (the official 12.3.4 image tag was not available), and PostgreSQL 19.0. All had 128 MiB of InnoDB buffer pool or PostgreSQL shared buffers, and used `READ COMMITTED`. InnoDB had `innodb_flush_log_at_trx_commit=1` with binlog disabled for both engines; PostgreSQL used `fsync=on`, `synchronous_commit=on` and `full_page_writes=on`. This tests standalone durable commits, not replication or MariaDB's newer optional binlog format. Do not compare its numbers to earlier MySQL runs as a version-only A/B: logging, isolation, and driver options differ.

The [HTTP verification](results/db-comparison-verify.jsonl) passed on the same Go binary across all three engines: 71 sync steps, 22 history-policy steps, eight SMTP recovery steps, and the public endpoint checks. A separate PostgreSQL smoke test also seeded and checked 501 history and favourites entries, crossing both SQL batch limits.

Four clients, 50 rows per account, 10 warmup and 50 timed operations per phase, three trials per engine. The load runs include a separate 500-row import. The memory runs exclude the import and measure sync after restarting only the Go process, so registration does not contribute to its RSS. The database stays warm. DB memory is the whole container cgroup's `anon + shmem`, not the parent process RSS or a sum of shared RSS; the raw data also records `memory.current` separately, which includes filesystem cache and other charges. Both are sampled, not memory limits.

Medians of the [load trials](results/db-comparison-load-four.jsonl). Throughput is completed operations/s; one favourites exchange is `GET` + `POST`.

| Engine | Changed history | History replay | Favourites exchange | Import 500 rows, ms |
|---|---:|---:|---:|---:|
| MySQL 9.7.4 | 258.2 | 261.6 | 237.8 | 176.5 |
| MariaDB 12.3.3 | 259.8 | 262.6 | 254.4 | 170.5 |
| PostgreSQL 19.0 | 250.3 | 257.9 | 226.1 | 89.7 |

| Engine | Changed history p95, ms | History replay p95, ms | Favourites exchange p95, ms |
|---|---:|---:|---:|
| MySQL 9.7.4 | 17.2 | 15.7 | 19.2 |
| MariaDB 12.3.3 | 16.6 | 16.7 | 16.4 |
| PostgreSQL 19.0 | 19.0 | 15.7 | 21.9 |

History throughput was close on all three. MariaDB led the favourites median; PostgreSQL completed the 500-row import in about half the time and led all three import trials. Only 20–24 reader operations completed during each import window; that is not enough to estimate a reader p95 under import.

[Post-restart memory](results/db-comparison-memory-four.jsonl), medians in MiB. The database stays running across the app restart; `memory.current` is for the database container only.

| Engine | Go peak RSS | DB idle anon + shmem | DB peak anon + shmem | DB peak memory.current |
|---|---:|---:|---:|---:|
| MySQL 9.7.4 | 28.7 | 441.5 | 445.3 | 469.5 |
| MariaDB 12.3.3 | 24.0 | 121.6 | 124.8 | 163.2 |
| PostgreSQL 19.0 | 25.7 | 27.2 | 33.9 | 104.0 |

PostgreSQL remained lowest in this small working set even including page cache and other cgroup charges. A configured 128 MiB cache is not necessarily fully resident. These measurements do not establish the memory needed for a larger catalogue or production concurrency.

## Kotlin 2.4.21 / Ktor 3.6.1 upgrade

This historical A/B compares Kotlin 2.2.20 / Ktor 3.3.0 (`b040cde59c`) with the upgraded port (`179f72fd77`). Run the recipe at `191016c00b`; the current port adds a shared API limiter and is no longer a dependency-only comparison. Build Netty and CIO from both historical trees with the same JDK 21 and GraalVM 25.0.5; the AOT targets also produce the JIT distributions.

The resolved Netty version moves from 4.2.4.Final to 4.2.17.Final, kotlinx-coroutines from 1.10.2 to 1.11.0 and kotlinx-serialization from 1.9.0 to 1.11.0. The SQLite and MySQL driver versions do not change. The result measures the whole stack upgrade, not a Kotlin compiler effect in isolation.

[HTTP verification](results/kotlin-stack-ab-verified.jsonl) passed on all 16 combinations: both versions, both engines, JIT and Native Image, SQLite and MySQL. Netty Native Image remains experimental; [Ktor documents CIO as the supported engine](https://ktor.io/docs/graalvm.html).

```bash
# Run from each checkout with JDK21_HOME and GRAALVM_HOME set.
JAVA_HOME="$JDK21_HOME" ./bench/build.sh tools kotlin-aot kotlin-cio-aot

# From the upgraded checkout only:
export JAVA_HOME="$GRAALVM_HOME"
control=/absolute/path/to/control-checkout
cpus=(--app-cpus 0-3 --mysql-cpus 4-5 --client-cpus 6-7)
python3 bench/kotlin-stack-ab.py "$control" verify "${cpus[@]}" \
  --output build/bench/kotlin-stack-verify.jsonl
python3 bench/kotlin-stack-ab.py "$control" memory "${cpus[@]}" \
  --clients 4 --library 50 --warmup 10 --rounds 50 --trials 3 \
  --output build/bench/kotlin-stack-memory-four.jsonl
```

The rate limit configuration, storage, and workload stay the same. This profile records throughput and p95 during authenticated sync after app restart. It excludes the neighbouring import and account creation; the separate MySQL process remains warm. Do not compare these figures directly to earlier load-only profiles. `old-` identifies the control; each JSONL record identifies the actual executable or JIT classpath by SHA-256.

Medians of three trials per cell, control → upgrade. See [all 48 trials](results/kotlin-stack-ab-memory-four.jsonl).

| Engine / build | DB | Changed history, ops/s | Replay, ops/s | Favourites, exchanges/s | Changed history p95, ms | App peak RSS, MiB |
|---|---|---:|---:|---:|---:|---:|
| Netty JIT | SQLite | 163.1 → 160.4 | 168.9 → 171.9 | 153.3 → 155.1 | 30.4 → 31.3 | 278.6 → 275.8 |
| Netty JIT | MySQL | 178.1 → 192.3 | 221.9 → 224.9 | 175.8 → 175.3 | 30.6 → 30.0 | 433.9 → 434.3 |
| CIO JIT | SQLite | 162.2 → 163.3 | 170.2 → 171.0 | 154.8 → 155.2 | 31.9 → 31.7 | 271.2 → 266.4 |
| CIO JIT | MySQL | 188.5 → 193.4 | 218.0 → 222.6 | 171.1 → 168.9 | 30.0 → 26.5 | 417.8 → 276.7 |
| Netty AOT | SQLite | 166.0 → 166.7 | 166.5 → 166.4 | 157.4 → 154.5 | 31.6 → 31.0 | 112.4 → 114.8 |
| Netty AOT | MySQL | 221.6 → 218.2 | 224.7 → 222.7 | 176.7 → 156.8 | 27.3 → 26.1 | 117.0 → 119.7 |
| CIO AOT | SQLite | 168.9 → 166.0 | 168.2 → 168.0 | 158.5 → 159.3 | 29.8 → 30.6 | 101.4 → 103.0 |
| CIO AOT | MySQL | 216.5 → 215.0 | 217.6 → 237.4 | 151.9 → 179.8 | 25.3 → 25.1 | 105.2 → 107.4 |

On SQLite, throughput stayed within 2%. On MySQL, upgraded Netty JIT improved changed-history throughput in all three trials (median +8%), and CIO AOT improved replay in all three (+9%). The MySQL favourites medians moved in opposite directions for the two native images; the trial ranges overlap, so +18% for CIO and −11% for Netty are not general performance claims. MySQL CIO JIT RSS moved from 274–421 MiB across the control trials to 277–281 MiB across the upgrade trials; its median fell, but the lowest control trial was already in the same range.

## Common Argon2id password profile

New passwords in all five ports use the original server's Argon2id v19 profile: 65,536 KiB of working memory per hash (total, not per lane), three passes, four lanes, a random 16-byte salt and a 32-byte digest. This matches the memory-constrained recommendation in [RFC 9106 §7.4](https://www.rfc-editor.org/rfc/rfc9106.html#section-7.4). The standard PHC encoding aligns the hash parameters, not implementation, threading or scheduling costs. Reset tokens still use their existing token hash.

Use fresh databases when comparing registrations or logins. Older C# PBKDF2 and Crystal bcrypt hashes require a password reset; Axum can still verify its existing Argon2id hashes at their stored costs. Historical results use the password schemes recorded for those snapshots. The post-restart sync profile continues to exclude password hashing regardless of scheme.

The [changed builds passed the live HTTP contract](results/argon2id-verified.jsonl) on SQLite and MySQL, including SMTP recovery and C# NativeAOT. A separate live SQLite check covered nine build variants: inspect the PHC from signup, verify it against libargon2 with a UTF-8 password, then check that each variant accepts a hash produced by Go and rejects a wrong password.
