#!/usr/bin/env bash
# Generates secrets once; existing non-empty files are kept.
set -euo pipefail
cd "$(dirname "$0")"
umask 077
mkdir -p secrets/devices
chmod 700 secrets secrets/devices   # host users can't enter the dir

rand() { local s; s="$(openssl rand -base64 96 | tr -dc 'A-Za-z0-9')"; printf '%s' "${s:0:$1}"; }

write() {  # name value
  local f="secrets/$1"
  if [[ -s "$f" ]]; then echo "kept  $f"; return; fi
  printf '%s' "$2" > "$f"
  chmod 644 "$f"   # readable by container UIDs (grafana=472); dir stays 700 on host
  echo "new   $f"
}

write influxdb_admin_password "$(rand 32)"
write influxdb_admin_token    "$(openssl rand -hex 32)"
write grafana_admin_password  "$(rand 32)"

# Placeholder; bootstrap.sh fills it with a read-only InfluxDB token for Grafana.
[[ -e secrets/grafana_influx_token ]] || { : > secrets/grafana_influx_token; chmod 644 secrets/grafana_influx_token; }
