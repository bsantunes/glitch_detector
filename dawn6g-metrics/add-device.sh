#!/usr/bin/env bash
# Usage: ./add-device.sh <device-id>
# Creates a write-only token for radio/network/events/location, stored in secrets/devices/<id>.token
source "$(dirname "$0")/lib.sh"

id="${1:?usage: $0 <device-id>}"
[[ "$id" =~ ^[A-Za-z0-9._-]{1,64}$ ]] || die "device id: letters, digits, . _ - only"
tok="secrets/devices/$id.token"
[[ -e "$tok" ]] && die "$id already has a token (revoke it first)"

args=()
for b in radio network events location; do
  bid="$(bucket_id "$b")"; [[ -n "$bid" ]] || die "bucket $b missing, run ./bootstrap.sh"
  args+=(--write-bucket "$bid")
done

json="$(ix auth create --description "device:$id" "${args[@]}" --json)"
token="$(json_field token <<<"$json")"
authid="$(json_field id <<<"$json")"
[[ -n "$token" && -n "$authid" ]] || die "token creation failed"

umask 077
printf '%s' "$token"  > "$tok"
printf '%s' "$authid" > "secrets/devices/$id.authid"
echo "device $id: write token in $tok (auth id $authid)"
