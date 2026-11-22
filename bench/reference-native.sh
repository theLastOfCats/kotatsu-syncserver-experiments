#!/usr/bin/env bash
# Build the historical reference JAR, retaining Ktor's Netty EngineMain.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
if (($# != 1)); then
    printf 'Usage: GRAALVM_HOME=/path/to/oracle-graalvm-25.0.5 %s /path/to/reference.jar\n' "$0" >&2
    exit 2
fi
: "${GRAALVM_HOME:?Set GRAALVM_HOME to Oracle GraalVM 25.0.5+9.1}"
jar=$(realpath "$1")
graal_home=$(realpath "$GRAALVM_HOME")
native_image="$graal_home/bin/native-image"
if [[ ! -r "$jar" || ! -x "$native_image" || ! -x "$graal_home/bin/javac" || ! -x "$graal_home/bin/jar" ]]; then
    printf 'Reference JAR or GraalVM build tools are unavailable\n' >&2
    exit 1
fi
# Keep ambient JVM options out of both version detection and the image build.
unset JAVA_TOOL_OPTIONS JDK_JAVA_OPTIONS _JAVA_OPTIONS CLASSPATH
expected_sha256=0c73622c015798bf3779c6dcbd65c930f851c7aac5e0f294c84faa90ef0fa0e1
actual_sha256=$(sha256sum < "$jar")
actual_sha256=${actual_sha256%% *}
if [[ "$actual_sha256" != "$expected_sha256" ]]; then
    printf 'Unexpected reference JAR SHA-256: %s\n' "$actual_sha256" >&2
    exit 1
fi
version=$("$native_image" --version)
if [[ "$version" != *'Oracle GraalVM 25.0.5+9.1'* ]]; then
    printf 'Oracle GraalVM 25.0.5+9.1 is required; got:\n%s\n' "$version" >&2
    exit 1
fi

printf '%s\n' 'Experimental: this original Ktor/Netty native build targets the MariaDB benchmark only; Netty AOT is not officially supported by Ktor.' >&2
mkdir -p "$root/build/bench"
work=$(mktemp -d "$root/build/bench/.reference-aot.XXXXXXXX")
trap 'rm -rf -- "$work"' EXIT
mkdir -p "$work/tmp" "$work/classes/META-INF/reference-native" "$work/classes/META-INF/services"
# Flyway's classpath scanner cannot enumerate Graal's resource: URLs. Register an
# image-only handler with an index derived from this exact JAR, not a copy of its SQL.
"$graal_home/bin/javac" --add-modules org.graalvm.nativeimage -cp "$jar" -d "$work/classes" \
    "$root/bench/reference-native/ReferenceNativeMigrations.java" \
    "$root/bench/reference-native/ReferenceKtormSubstitutions.java"
"$graal_home/bin/jar" tf "$jar" | grep -E '^db/migration/.*[^/]$' | LC_ALL=C sort -u \
    > "$work/classes/META-INF/reference-native/migrations.list"
printf '%s\n' 'org.kotatsu.bench.reference.ReferenceNativeMigrations' \
    > "$work/classes/META-INF/services/org.flywaydb.core.extensibility.Plugin"
(
    cd "$work"
    export TMPDIR="$work/tmp"
    # Metadata traces reference sync and SMTP recovery, plus bundled resources.
    "$native_image" \
        -J-Xmx4g -J-XX:ActiveProcessorCount=4 --parallelism=4 \
        --no-fallback \
        -H:+UnlockExperimentalVMOptions -H:+SharedArenaSupport -H:-UnlockExperimentalVMOptions \
        --exclude-config '.*' 'META-INF/native-image/io\.netty/netty-codec-http2?/native-image\.properties' \
        --initialize-at-run-time=org.kotatsu,org.slf4j,ch.qos.logback,io.netty \
        -H:ConfigurationFileDirectories="$root/bench/reference-native" \
        -cp "$work/classes:$jar" \
        -o "$work/kotatsu-syncserver" io.ktor.server.netty.EngineMain
)
# Publish only after a completed ELF; retain Native Image's JDK companion libraries.
readelf -h "$work/kotatsu-syncserver" >/dev/null
out="$root/build/bench/reference-aot"
mkdir -p "$out"
for library in "$work"/*.so; do
    [[ -f "$library" ]] || continue
    chmod 755 "$library"
    mv -f "$library" "$out/"
done
chmod 755 "$work/kotatsu-syncserver"
mv -f "$work/kotatsu-syncserver" "$out/kotatsu-syncserver"
printf 'Built %s\n' "$out/kotatsu-syncserver"
