#!/usr/bin/env bash
# Usage: ./revoke-device.sh <device-id>
source "$(dirname "$0")/lib.sh"

id="${1:?usage: $0 <device-id>}"
f="secrets/devices/$id.authid"
[[ -s "$f" ]] || die "no auth id recorded for $id"
ix auth delete --id "$(cat "$f")" >/dev/null
rm -f "$f" "secrets/devices/$id.token"
echo "revoked $id"
