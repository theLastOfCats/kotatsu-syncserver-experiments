# Kotatsu Syncserver Experiments

Five experimental ports of the [Kotatsu Android reader’s sync server](https://github.com/theLastOfCats/kotatsu-syncserver): **Go, C# / ASP.NET Core, Kotlin / Ktor, Rust / Axum, and Crystal**. They implement the same HTTP routes for accounts, reading history, favourites and categories. Each port has its own storage schema and runs against SQLite or MySQL; database files are not interchangeable.

This project asks two questions: **How much faster or lighter can the sync server be without changing what the client sees?** And **how far can an AI coding agent get porting a real service across stacks?** The ports were built and optimized with AI assistance. We check the result against a running server: response snapshots, multi-device sync, account isolation, and password recovery through local SMTP. Compilation alone does not count as compatibility.

These are research and benchmarking implementations, **not official Kotatsu releases or production deployment recommendations**. Measurements compare implementations and configurations, not languages in isolation. JIT and AOT builds are reported separately.

## How the stacks compare

Historical runs of the same authenticated sync workload: four clients, 50 history and 50 favourite entries per account, medians of three trials. All columns come from the post-restart profile: the clients reuse their JWTs, with no registration, password hashing, or neighbouring import.

| Stack and build | Database | History update, ops/s ↑ | Update p95, ms ↓ | History replay, ops/s ↑ | Favourites exchanges/s ↑ | App idle RSS, MiB ↓ | App peak RSS, MiB ↓ | DB peak RSS, MiB ↓ |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| Go · `net/http` | MySQL | 104.4 | 47.0 | 103.3 | 85.4 | 14.8 | 25.4 | 472.3 |
| C# · ASP.NET Core JIT | MySQL | 173.9 | 30.6 | 177.4 | 165.4 | 101.5 | 140.8 | 475.2 |
| C# · ASP.NET Core NativeAOT | MySQL | 223.2 | 23.5 | 215.0 | 207.1 | 51.6 | 93.1 | 477.1 |
| Kotlin · Ktor Netty JIT | MySQL | 192.3 | 30.0 | 224.9 | 175.3 | 224.7 | 434.3 | 470.4 |
| Kotlin · Ktor Netty Native Image | MySQL | 218.2 | 26.1 | 222.7 | 156.8 | 66.1 | 119.7 | 470.3 |
| Kotlin · Ktor CIO JIT | MySQL | 193.4 | 26.5 | 222.6 | 168.9 | 184.8 | 276.7 | 470.3 |
| Kotlin · Ktor CIO Native Image | MySQL | 215.0 | 25.1 | **237.4** | 179.8 | 57.4 | 107.4 | 470.2 |
| Rust · Axum | MySQL | **226.6** | **22.2** | 235.5 | **223.7** | **8.1** | **11.7** | 484.8 |
| Crystal · `HTTP::Server` | MySQL | 120.1 | 40.2 | 119.3 | 58.8 | 14.0 | 21.1 | 475.1 |
| Original Ktor Netty · Temurin 21 JIT | MariaDB | 33.2 | 133.1 | 34.3 | 26.6 | 225.3 | 421.2 | 154.0 |
| Original Ktor Netty · GraalVM 25 JIT | MariaDB | 33.3 | 137.2 | 35.0 | 27.3 | 215.4 | 479.4 | 153.9 |
| Original Ktor Netty · GraalVM 25 Native Image | MariaDB | 29.5 | 151.8 | 30.0 | 23.6 | 87.0 | 132.5 | 153.6 |

One favourites exchange is a GET plus a POST. RSS is sampled per process, not whole-system memory. The ports use MySQL 8.4.13; the original uses MariaDB 11.8.9 with different durability settings. Compare complete stacks, not languages in isolation. These builds predate the unified API limits.

[Raw results (including SQLite)](bench/results/) · [Methodology](bench/README.md#memory-of-an-already-authenticated-sync) · [Kotlin 2.4.21 / Ktor 3.6.1 trials](bench/results/kotlin-stack-ab-memory-four.jsonl)

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
