# Kotatsu Syncserver Experiments

Five experimental ports of the [Kotatsu Android reader’s sync server](https://github.com/theLastOfCats/kotatsu-syncserver): **Go, C# / ASP.NET Core, Kotlin / Ktor, Rust / Axum, and Crystal**. They implement the same HTTP routes for accounts, reading history, favourites and categories. Each port has its own storage schema and runs against SQLite or MySQL; database files are not interchangeable.

This project asks two questions: **How much faster or lighter can the sync server be without changing what the client sees?** And **how far can an AI coding agent get porting a real service across stacks?** The ports were built and optimized with AI assistance. We check the result against a running server: response snapshots, multi-device sync, account isolation, and password recovery through local SMTP. Compilation alone does not count as compatibility.

These are research and benchmarking implementations, **not official Kotatsu releases or production deployment recommendations**. Measurements compare implementations and configurations, not languages in isolation. JIT and AOT builds are reported separately.

## How the stacks compare

Benchmarks with refreshed port dependencies: four clients, 50 history and 50 favourite entries per account, medians of three trials. All columns come from the post-restart profile: the clients reuse their JWTs, with no registration, password hashing, or neighbouring import.

One favourites exchange is a GET plus a POST. RSS is sampled per process, not whole-system memory. All builds ran with the same API ceiling of 1,000 requests/s. These runs compare complete stacks, not individual languages or the effect of a dependency upgrade.

### SQLite

SQLite runs inside the application, so app RSS includes the database engine. There is no separate DB process.

| Stack and build | History update, ops/s ↑ | Update p95, ms ↓ | History replay, ops/s ↑ | Favourites exchanges/s ↑ | App idle RSS, MiB ↓ | App peak RSS, MiB ↓ |
|---|---:|---:|---:|---:|---:|---:|
| Go · `net/http` | 152.6 | 39.0 | 152.5 | 140.2 | 16.6 | 27.6 |
| C# · ASP.NET Core JIT | 115.7 | 8.8 | 118.4 | 109.6 | 80.9 | 118.8 |
| C# · ASP.NET Core NativeAOT | 126.7 | **7.3** | 135.7 | 111.8 | 40.6 | 60.1 |
| Kotlin · Ktor Netty JIT | 166.1 | 30.5 | 172.7 | 155.4 | 201.6 | 281.3 |
| Kotlin · Ktor Netty Native Image | 169.2 | 30.2 | 169.2 | 161.4 | 67.4 | 119.4 |
| Kotlin · Ktor CIO JIT | 166.8 | 30.6 | 173.6 | 157.6 | 208.6 | 267.7 |
| Kotlin · Ktor CIO Native Image | 167.8 | 30.0 | 169.3 | 161.5 | 57.7 | 108.0 |
| Rust · Axum | **182.2** | 22.3 | **180.2** | **164.8** | **9.0** | **12.3** |
| Crystal · `HTTP::Server` | 163.8 | 25.2 | 163.5 | 69.4 | 10.2 | 15.7 |

### MySQL 8.4.13

MySQL runs in a separate process. The original Ktor server is not a port and was measured on MariaDB instead; see the [reference results](bench/README.md#original-ktor-reference-on-mariadb).

| Stack and build | History update, ops/s ↑ | Update p95, ms ↓ | History replay, ops/s ↑ | Favourites exchanges/s ↑ | App idle RSS, MiB ↓ | App peak RSS, MiB ↓ | DB peak RSS, MiB ↓ |
|---|---:|---:|---:|---:|---:|---:|---:|
| Go · `net/http` | 121.6 | 40.6 | 118.3 | 108.3 | 15.1 | 24.2 | 473.1 |
| C# · ASP.NET Core JIT | 211.7 | 24.9 | 224.0 | 164.9 | 102.2 | 143.9 | 477.1 |
| C# · ASP.NET Core NativeAOT | 197.6 | 25.6 | 233.6 | 217.0 | 51.4 | 88.7 | 475.2 |
| Kotlin · Ktor Netty JIT | 190.0 | 25.9 | 231.4 | 175.0 | 225.1 | 435.6 | 472.2 |
| Kotlin · Ktor Netty Native Image | 220.9 | 26.2 | 234.1 | 182.7 | 72.0 | 124.1 | 470.4 |
| Kotlin · Ktor CIO JIT | 194.1 | 30.1 | 229.6 | 185.7 | 203.1 | 277.4 | 470.4 |
| Kotlin · Ktor CIO Native Image | 215.8 | 25.7 | 222.3 | 180.2 | 62.3 | 112.5 | 470.2 |
| Rust · Axum | **231.3** | **21.6** | **235.6** | **229.5** | **8.5** | **12.6** | 484.6 |
| Crystal · `HTTP::Server` | 168.0 | 35.1 | 177.0 | 63.5 | 14.6 | 21.7 | 476.6 |

[All 54 trials](bench/results/current-stacks-memory-four.jsonl) · [Dependency versions and reproduction](bench/README.md#current-port-comparison)

## Same Go backend, different databases

Separate experiment: the same Go binary, four clients and three trials per engine. All three ran with a 128 MiB database cache setting and durable standalone commits. SQLite is in-process and was not part of this experiment.

| Database | History update, ops/s ↑ | Update p95, ms ↓ | History replay, ops/s ↑ | Favourites exchanges/s ↑ | 500-row import, ms ↓ | App peak RSS, MiB ↓ | DB peak anon + shmem, MiB ↓ | DB peak incl. cache, MiB ↓ |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| MySQL 9.7.4 | 258.2 | 17.2 | 261.6 | 237.8 | 176.5 | 28.7 | 445.3 | 469.5 |
| MariaDB 12.3.3 | 259.8 | 16.6 | 262.6 | 254.4 | 170.5 | 24.0 | 124.8 | 163.2 |
| PostgreSQL 19.0 | 250.3 | 19.0 | 257.9 | 226.1 | **89.7** | 25.7 | **33.9** | **104.0** |

Speed comes from the load trials; memory comes from separate post-restart trials without registration or import. DB memory here is for the whole container (`anon + shmem` and `memory.current`), not the per-process RSS used above. PostgreSQL was an opt-in experiment, not a production storage option.

[Methodology](bench/README.md#one-go-backend-on-mysql-mariadb-and-postgresql) · [Load trials](bench/results/db-comparison-load-four.jsonl) · [Memory trials](bench/results/db-comparison-memory-four.jsonl)

## Try a port

From the repository root, with Go 1.26+ and the selected port’s toolchain installed:

```sh
./bench/build.sh tools go
export JWT_SECRET="$(openssl rand -hex 32)"
export PORT=8080 BASE_URL=http://127.0.0.1:8080
export DATABASE_DIALECT=sqlite DATABASE_PATH="$PWD/build/demo.sqlite"
./build/bench/go
```

In another terminal, `curl http://127.0.0.1:8080/` should return `Alive`. See [build and runtime instructions](bench/README.md#build-and-run) for the other ports, Kotlin’s Netty/CIO builds, NativeAOT and MySQL. Use a disposable database and keep the JWT secret stable across restarts if sessions should survive.

All five ports create Argon2id password hashes with the same cost profile. Use a fresh database for comparisons: the old C# PBKDF2 and Crystal bcrypt hashes are not accepted; existing accounts on those builds need a password reset. See [benchmark methodology](bench/README.md#common-argon2id-password-profile).

All five ports share the same [rate limits](bench/README.md#rate-limits). JWT sync uses only the shared API ceiling and never uses a password slot.

## Tests and results

- [HTTP contract](contract-tests/README.md): what the Android-facing scenarios cover and how to run them.
- [Benchmarks](bench/README.md): repeatable load and memory profiles, optimization A/B results, and raw JSONL. The post-restart memory profile excludes registration.
- [Opt-in experiments](experiments/README.md): run one Go backend on MySQL, MariaDB and PostgreSQL; reproduce the separately tested original Ktor variants.

Extracted from `kotatsu-syncserver` at `87bf921c86`. The original server is not included here. Distributed under the [GNU GPL v3](LICENSE).
