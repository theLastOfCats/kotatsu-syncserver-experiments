#!/usr/bin/env bash
# Build the CIO distribution without changing the default Netty build.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
"$root/gradlew" -p "$root/apps/kotlin" --console=plain -PserverEngine=cio installDist
