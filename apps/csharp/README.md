# Kotatsu C# service

A .NET 11 implementation of the Kotatsu sync API. It stores data in SQLite or MySQL 8.4 and runs in the foreground. A recent .NET 11 SDK is required to build; the published executable is named `Kotatsu`.

```sh
cd apps/csharp
dotnet publish -c Release -o out
JWT_SECRET='replace-with-a-long-random-secret' DATABASE_PATH=./kotatsu.db ./out/Kotatsu
```

For a separate Linux x86-64 NativeAOT build, from the repository root run:

```sh
./bench/build.sh csharp-aot
JWT_SECRET='replace-with-a-long-random-secret' DATABASE_PATH=./kotatsu.db ./build/bench/csharp-aot/Kotatsu
```

This publishes an optimized, self-contained .NET 11 native executable without replacing the default JIT build.
It requires the .NET 11 SDK, the .NET 11 NativeAOT compiler and Linux runtime packs (restored from NuGet),
and a C toolchain with zlib development headers. Keep the published `libe_sqlite3.so` beside the executable;
the native app also uses the host's standard system libraries. Both SQLite and MySQL and console and SMTP
mail use the same configuration as the JIT build.

By default the server listens on `127.0.0.1:8080`, accepts new registrations and writes recovery mail to the console. Set `PORT` to change the port, or `BIND_HOST` and `PORT` to bind explicitly to another interface. `ASPNETCORE_URLS` (or `URLS`) is honored when `PORT` is not set. Set `BASE_URL` to the externally reachable HTTP(S) URL when using a reverse proxy; it is used in password-recovery links. Terminate TLS at a trusted proxy for network access. Do not expose the console mail log: it contains live reset links.

Responses support gzip when the client advertises it in `Accept-Encoding`.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `JWT_SECRET` | required | Stable, high-entropy signing secret. Changing it invalidates existing sessions. |
| `JWT_ISSUER`, `JWT_AUDIENCE` | reference server values | Optional JWT claim overrides; keep stable across restarts. |
| `ALLOW_NEW_REGISTER` | `true` | Set to `false` to permit only existing accounts. |
| `DATABASE_DIALECT` | `sqlite` | `sqlite` or `mysql`. |
| `DATABASE_PATH` | `kotatsu.db` | SQLite database file, relative to the server's working directory unless absolute. |
| `DATABASE_HOST`, `DATABASE_PORT` | `127.0.0.1`, `3306` | MySQL address. |
| `DATABASE_NAME`, `DATABASE_USER`, `DATABASE_PASSWORD` | `kotatsu_db`, `kotatsu`, empty | MySQL database and credentials. Create the database before startup. |
| `BASE_URL` | `http://localhost:<PORT>` | Public URL used in recovery mail and app deep links; without credentials, query or fragment. |
| `MAIL_PROVIDER` | `console` | `console` or `smtp`. |
| `SMTP_HOST`, `SMTP_PORT`, `SMTP_FROM` | host/from required; port `587` | SMTP server and sender mailbox, e.g. `Kotatsu <no-reply@example.com>`. |
| `SMTP_USERNAME`, `SMTP_PASSWORD` | empty | Optional SMTP credentials; provide both or neither. |
| `SMTP_SECURITY` | `auto` | `auto` uses implicit TLS on port 465, requires STARTTLS with credentials, and otherwise uses STARTTLS when offered. `starttls` requires STARTTLS; `ssl` uses implicit TLS; `none` permits plaintext without credentials. |
| `RATE_LIMIT_API_PER_SECOND` | `200` | Shared API requests per second, across all clients. |
| `RATE_LIMIT_AUTH_PER_MINUTE` | `5` | Per-client-IP `/auth` requests per minute. |
| `RATE_LIMIT_FORGOT_PER_10_MINUTES` | `5` | Per-client-IP `/forgot-password` requests per 10 minutes. |
| `RATE_LIMIT_RESET_PER_10_MINUTES` | `5` | Per-client-IP `/reset-password` requests per 10 minutes. |
| `RATE_LIMIT_PASSWORD_CONCURRENCY` | `2` | Maximum concurrent password hashes and verifications. |

Limits must be positive integers. The shared fixed-window API limit applies to authentication, recovery, sync, profile and manga requests; health and deeplink routes are excluded. Authentication and recovery have additional per-route limits keyed by the directly connected IP, without trusting forwarded headers. If a reverse proxy is used, its address is therefore the shared client identity for these limits. Rejected requests return 429 with `Retry-After`. Password hashing and verification do not queue: when all password slots are occupied, requests return 503 with `Retry-After: 1`.

For local Mailpit without credentials, set `MAIL_PROVIDER=smtp`, `SMTP_HOST=127.0.0.1`, `SMTP_PORT=1025` and `SMTP_FROM=kotatsu@example.invalid`. The console sender and Mailpit are development facilities; production recovery links should use HTTPS and a trusted SMTP service with TLS.

Passwords use Argon2id v19 (64 MiB, 3 iterations, 4 lanes) with a random 16-byte salt and a 32-byte hash, stored in unpadded PHC format. Historical PBKDF2 password hashes are not supported by this experiment; existing accounts need a password reset. Reset tokens are random, stored only as hashes, expire after 15 minutes and are single-use. A successful reset invalidates existing JWT sessions. SQLite uses WAL and full synchronous commits; back up an active database with SQLite's online backup API rather than copying only the main file. Protect the database, JWT secret and backups as credentials.

For contract and restore-drill commands, see `contract-tests/README.md` in the repository root. Use disposable storage and a disposable local mailbox for those scenarios.
