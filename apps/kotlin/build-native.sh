#!/usr/bin/env bash
# Build the existing Ktor application as a separate GraalVM Native Image.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
: "${GRAALVM_HOME:?Set GRAALVM_HOME to a GraalVM installation with native-image}"
: "${JAVA_HOME:?Set JAVA_HOME to a JDK supported by Gradle}"
if [[ ! -x "$GRAALVM_HOME/bin/native-image" ]]; then
    printf 'native-image not found in GRAALVM_HOME: %s\n' "$GRAALVM_HOME" >&2
    exit 1
fi

case "${1:-netty}" in
    netty)
        "$root/gradlew" -p "$root/apps/kotlin" --console=plain installDist
        lib="$root/apps/kotlin/build/install/kotatsu-kotlin/lib"
        out="$root/build/bench/kotlin-aot"
        metadata="$root/apps/kotlin/native-image"
        # Netty's HTTP codecs request build-time initialization of all Netty,
        # conflicting with the runtime initialization required by other JARs.
        engine_options=(
            -H:+UnlockExperimentalVMOptions -H:+SharedArenaSupport -H:-UnlockExperimentalVMOptions
            --exclude-config '.*netty-codec-http2?-.*\.jar'
                'META-INF/native-image/io\.netty/netty-codec-http2?/native-image\.properties'
            --initialize-at-run-time=io.netty
        )
        ;;
    cio)
        "$root/apps/kotlin/build-cio.sh"
        lib="$root/build/bench/kotlin-cio/lib"
        out="$root/build/bench/kotlin-cio-aot"

        # The existing traces cover both databases, password hashing, and SMTP.
        # Retain their engine-independent entries but omit Netty reachability.
        filtered_metadata=$(mktemp -d "${TMPDIR:-/tmp}/kotatsu-cio-native.XXXXXX")
        engine_options=()
        trap 'rm -rf -- "$filtered_metadata"' EXIT
        python3 - "$root/apps/kotlin/native-image" "$filtered_metadata" <<'PY'
import json
import pathlib
import sys

source, target = map(pathlib.Path, sys.argv[1:])

def contains_netty(value):
    if isinstance(value, str):
        return "netty" in value.lower()
    if isinstance(value, list):
        return any(contains_netty(item) for item in value)
    if isinstance(value, dict):
        return any(contains_netty(item) for item in value.values())
    return False

for name in ("reflect-config.json", "jni-config.json", "proxy-config.json"):
    entries = json.loads((source / name).read_text())
    (target / name).write_text(json.dumps(
        [entry for entry in entries if not contains_netty(entry)]
    ))

resources = json.loads((source / "resource-config.json").read_text())
resources["resources"]["includes"] = [
    entry for entry in resources["resources"]["includes"] if not contains_netty(entry)
]
(target / "resource-config.json").write_text(json.dumps(resources))
PY
        metadata="$filtered_metadata,$root/apps/kotlin/native-image-cio"

        ;;
    *)
        printf 'Usage: %s [netty|cio]\n' "$0" >&2
        exit 2
        ;;
esac

jars=("$lib"/*.jar)
classpath=$(IFS=:; printf '%s' "${jars[*]}")
mkdir -p "$out"

"$GRAALVM_HOME/bin/native-image" \
    -J-Xmx4g -J-XX:ActiveProcessorCount=4 --parallelism=4 \
    --no-fallback \
    --initialize-at-run-time=kotatsu,org.slf4j,ch.qos.logback \
    "${engine_options[@]}" \
    -H:ConfigurationFileDirectories="$metadata" \
    -cp "$classpath" \
    -o "$out/kotatsu-kotlin" kotatsu.MainKt
