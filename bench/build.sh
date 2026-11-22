#!/usr/bin/env bash
# Run from any directory. Set JAVA_HOME to JDK 21 for the Kotlin build.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
out="$root/build/bench"
mkdir -p "$out"
if (($# == 0)); then set -- tools go csharp kotlin axum crystal; fi
for target do
    case "$target" in
        tools)
            (cd "$root/contract-tests" && go build -trimpath -o "$out/contract" .)
            (cd "$root/bench/load" && go build -trimpath -o "$out/load" .) ;;
        go) (cd "$root/apps/go" && go build -trimpath -o "$out/go" .) ;;
        csharp) dotnet publish "$root/apps/csharp" -c Release -o "$out/csharp" ;;
        csharp-aot) dotnet publish "$root/apps/csharp/Kotatsu.csproj" -c Release -r linux-x64 \
            --self-contained true -p:PublishAot=true -o "$out/csharp-aot" ;;
        kotlin) "$root/gradlew" -p "$root/apps/kotlin" --console=plain installDist ;;
        kotlin-aot) "$root/apps/kotlin/build-native.sh" ;;
        kotlin-cio) "$root/apps/kotlin/build-cio.sh" ;;
        kotlin-cio-aot) "$root/apps/kotlin/build-native.sh" cio ;;
        axum)
            cargo build --manifest-path "$root/apps/axum/Cargo.toml" --locked --release \
                --target-dir "$out/cargo"
            install -m 755 "$out/cargo/release/kotatsu-axum" "$out/axum" ;;
        crystal)
            (cd "$root/apps/crystal" && mise exec crystal@latest -- shards install --production &&
                mise exec crystal@latest -- crystal build src/server.cr --release -o "$out/crystal") ;;
        *) printf 'Unknown target: %s\n' "$target" >&2; exit 2 ;;
    esac
done
