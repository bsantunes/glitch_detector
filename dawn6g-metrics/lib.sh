# shellcheck shell=bash
# Shared helpers. Runs the influx CLI inside the container with the admin token
# read from the Docker secret, so the token never appears on the host command line.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
set -a; source ./.env; set +a

die() { echo "error: $*" >&2; exit 1; }

ix() {
  docker compose exec -T -e INFLUX_ORG="$INFLUX_ORG" influxdb \
    sh -c 'INFLUX_TOKEN="$(cat /run/secrets/influxdb_admin_token)" exec influx "$@"' influx "$@"
}

bucket_id() { ix bucket list -n "$1" --hide-headers 2>/dev/null | awk 'NR==1{print $1}' || true; }

ensure_bucket() {  # name retention
  if [[ -z "$(bucket_id "$1")" ]]; then
    ix bucket create -n "$1" -r "$2" >/dev/null && echo "created bucket $1 ($2)"
  else
    echo "bucket $1 exists"
  fi
}

json_field() { awk -F'"' -v k="$1" '$2==k {print $4; exit}'; }  # first top-level match
