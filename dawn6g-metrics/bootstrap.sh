#!/usr/bin/env bash
# First start: brings up InfluxDB, creates buckets and Grafana's read-only token, starts everything.
# Safe to re-run.
source "$(dirname "$0")/lib.sh"

[[ -s secrets/influxdb_admin_token ]] || die "run ./gen-secrets.sh first"

docker compose up -d --wait influxdb

ensure_bucket radio   "$RETENTION_RADIO"
ensure_bucket network "$RETENTION_NETWORK"
ensure_bucket events  "$RETENTION_EVENTS"

if [[ -s secrets/grafana_influx_token ]]; then
  echo "grafana token exists"
else
  args=()
  for b in radio network events; do args+=(--read-bucket "$(bucket_id "$b")"); done
  token="$(ix auth create --description "grafana-read" "${args[@]}" --json | json_field token)"
  [[ -n "$token" ]] || die "could not create grafana token"
  printf '%s' "$token" > secrets/grafana_influx_token
  echo "created grafana read-only token"
fi

docker compose up -d
echo
echo "Grafana : https://${DOMAIN}/grafana/   (user ${GRAFANA_ADMIN_USER}, password in secrets/grafana_admin_password)"
echo "InfluxDB: https://${DOMAIN}:8086       (user ${INFLUX_ADMIN_USER}, password in secrets/influxdb_admin_password)"
