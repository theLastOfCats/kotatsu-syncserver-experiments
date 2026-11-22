# Opt-in experiments

## One Go binary on MySQL, MariaDB and PostgreSQL

[`go-cross-engine.patch`](go-cross-engine.patch) is the exact experimental source change from the original `d9e7c72239` checkout. It adds a PostgreSQL adapter to the Go port and the three-engine benchmark runner. It is not part of the default Go build. Apply it in a disposable checkout of this repository, starting with the imported source:

```sh
git apply experiments/go-cross-engine.patch
./bench/build.sh tools go
podman pull docker.io/library/mysql:9.7.4
podman pull docker.io/library/mariadb:12.3.3
podman pull docker.io/library/postgres:19.0
podman pull docker.io/axllent/mailpit:latest
python3 bench/database_matrix.py verify --trials 1 --output build/bench/db-verify.jsonl
python3 bench/database_matrix.py load --output build/bench/db-load.jsonl
python3 bench/database_matrix.py memory --output build/bench/db-memory.jsonl
```

See the [three-engine report](../bench/README.md#one-go-backend-on-mysql-mariadb-and-postgresql) for versions, durability settings and what the memory measurements count. Adjust the CPU lists if your host does not match the documented setup. The runner starts disposable loopback containers and will not overwrite output files.

## Original Ktor baseline

The original source, container image and JAR are external inputs, separate from the port under `apps/kotlin`. For a new reference run, check out commit `125ff524a466b75885d2010b220cb71f404d3b69` of [the original repository](https://github.com/theLastOfCats/kotatsu-syncserver) in a separate directory. In that checkout, apply [`reference-load-only-rate-limit.patch`](reference-load-only-rate-limit.patch) from this repository. It enables a load-only rate limit override and reproduces the source of the measured `700d5c6` reference. Leave the override unset in contract verification and outside disposable load tests.

Build the shaded JAR with JDK 21 and `./gradlew shadowJar` in the original checkout, then pass its absolute path to `bench/reference.py` here. A rebuild need not be byte-identical to the archived JAR used for the published results. The original Netty Native Image helper requires that exact archived JAR, which is not included. For the isolated tag optimization, [`reference-tags-once.patch`](reference-tags-once.patch) records the source difference; the published JIT result used a copy of the archived JAR with only the recompiled `ModelMappingKt.class` replaced.
