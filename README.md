# Kotatsu Syncserver Experiments

Five experimental ports of the [Kotatsu Android reader’s sync server](https://github.com/theLastOfCats/kotatsu-syncserver): **Go, C# / ASP.NET Core, Kotlin / Ktor, Rust / Axum, and Crystal**. They implement the same HTTP routes for accounts, reading history, favourites and categories. Each port has its own storage schema and runs against SQLite or MySQL; database files are not interchangeable.

This project asks two questions: **How much faster or lighter can the sync server be without changing what the client sees?** And **how far can an AI coding agent get porting a real service across stacks?** The ports were built and optimized with AI assistance. We check the result against a running server: response snapshots, multi-device sync, account isolation, and password recovery through local SMTP. Compilation alone does not count as compatibility.

These are research and benchmarking implementations, **not official Kotatsu releases or production deployment recommendations**. Measurements compare implementations and configurations, not languages in isolation. JIT and AOT builds are reported separately.

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

## Tests and results

- [HTTP contract](contract-tests/README.md): what the Android-facing scenarios cover and how to run them.
- [Benchmarks](bench/README.md): repeatable load and memory profiles, optimization A/B results, and raw JSONL. The post-restart memory profile excludes registration.
- [Opt-in experiments](experiments/README.md): run one Go backend on MySQL, MariaDB and PostgreSQL; reproduce the separately tested original Ktor variants.

Extracted from `kotatsu-syncserver` at `87bf921c86`. The original server is not included here. Distributed under the [GNU GPL v3](LICENSE).
