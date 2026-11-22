#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 || ( $1 != record && $1 != verify ) ]]; then
    printf 'Usage: REFERENCE_IMAGE=<local-image> ./reference.sh {record|verify} [sync|recovery|observations]\n' >&2
    exit 2
fi
mode=$1
suite=${2:-sync}
case "$suite" in
    sync|recovery|observations) ;;
    *) printf 'Unknown suite: %s\n' "$suite" >&2; exit 2 ;;
esac
: "${REFERENCE_IMAGE:?Set REFERENCE_IMAGE to a locally built Kotatsu Syncserver image}"
db_image=${DB_IMAGE:-docker.io/library/mariadb:11.8}
mail_image=${MAILPIT_IMAGE:-docker.io/axllent/mailpit:latest}
cd -- "$(dirname -- "$0")"

# Never pull or reuse an existing deployment. API ports are random and loopback-only.
podman image exists "$REFERENCE_IMAGE"
podman image exists "$db_image"
if [[ $suite != sync ]]; then podman image exists "$mail_image"; fi
jwt_secret=$(openssl rand -hex 32)
network= db= backend= mail=
cleanup() {
    if [[ -n $backend ]]; then podman rm -f -v "$backend" >/dev/null || true; fi
    if [[ -n $mail ]]; then podman rm -f -v "$mail" >/dev/null || true; fi
    if [[ -n $db ]]; then podman rm -f -v "$db" >/dev/null || true; fi
    if [[ -n $network ]]; then podman network rm "$network" >/dev/null || true; fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

network=$(podman network create)
db=$(podman run --pull=never -d --network "$network" --network-alias db \
    --label "io.kotatsu.contract-fixture=$suite" \
    -e MARIADB_RANDOM_ROOT_PASSWORD=1 -e MARIADB_DATABASE=kotatsu_contract \
    -e MARIADB_USER=kotatsu -e MARIADB_PASSWORD=local-contract-only "$db_image")

wait_until() {
    local attempt
    for ((attempt = 0; attempt < 90; attempt++)); do
        if "$@"; then return 0; fi
        sleep 1
    done
    return 1
}
db_ready() {
    podman exec -e MYSQL_PWD=local-contract-only "$db" \
        mariadb --protocol=tcp -h 127.0.0.1 -u kotatsu kotatsu_contract -e 'SELECT 1' >/dev/null 2>&1
}
if ! wait_until db_ready; then
    podman logs "$db" >&2
    exit 1
fi

published_origin() {
    local address
    if ! address=$(podman port "$1" "$2/tcp") || [[ ! $address =~ ^127\.0\.0\.1:[0-9]+$ ]]; then
        printf 'No loopback port available for %s/tcp\n' "$2" >&2
        podman logs "$1" >&2
        return 1
    fi
    printf 'http://%s\n' "$address"
}

backend_options=()
runner_options=(--suite "$suite")
if [[ $suite != sync ]]; then
    mail=$(podman run --pull=never -d --network "$network" --network-alias mailpit -p 127.0.0.1::8025 \
        -e MP_SMTP_AUTH=kotatsu:local-contract-only -e MP_SMTP_AUTH_ALLOW_INSECURE=true "$mail_image")
    mail_url=$(published_origin "$mail" 8025)
    mail_ready() {
        curl --max-time 2 -fsS "$mail_url/api/v1/messages" >/dev/null 2>&1
    }
    if ! wait_until mail_ready; then
        podman logs "$mail" >&2
        exit 1
    fi
    backend_options+=(-e MAIL_PROVIDER=smtp -e SMTP_HOST=mailpit -e SMTP_PORT=1025 \
        -e SMTP_USERNAME=kotatsu -e SMTP_PASSWORD=local-contract-only -e SMTP_FROM=kotatsu@example.invalid)
    runner_options+=(--mailpit-url "$mail_url")
fi
if [[ $suite == observations ]]; then runner_options+=(--reference-db "$db"); fi

backend=$(podman run --pull=never -d --network "$network" -p 127.0.0.1::8080 \
    -e DATABASE_HOST=db -e DATABASE_NAME=kotatsu_contract -e DATABASE_DIALECT=mariadb \
    -e DATABASE_USER=kotatsu -e DATABASE_PASSWORD=local-contract-only \
    -e JWT_SECRET="$jwt_secret" -e ALLOW_NEW_REGISTER=true "${backend_options[@]}" "$REFERENCE_IMAGE")
base_url=$(published_origin "$backend" 8080)
api_ready() {
    curl --max-time 2 -fsS "$base_url/" >/dev/null 2>&1
}
if ! wait_until api_ready; then
    podman logs "$backend" >&2
    exit 1
fi

go run . "$mode" --base-url "$base_url" "${runner_options[@]}"
