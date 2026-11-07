#!/usr/bin/env bash
# Deploy wg6pn into a sprite as a sprite-env service.
#
#   SPRITE=my-sprite ORG=my-org CONNECTOR=<connection-id> ./deploy.sh
#
# Optional:
#   SPRITES_ORG   sprites org for the sprite CLI (default: $ORG)
#   NETWORK       custom 6PN name (default: the org's default network)
#   FORWARD       inbound ports, "8080" or "8080:3000,5432" (default: 8080)
#   CREATE=1      create the sprite first
#
set -euo pipefail

: "${SPRITE:?set SPRITE}" "${ORG:?set ORG to the org slug}" "${CONNECTOR:?set CONNECTOR to the connector id}"
SPRITES_ORG=${SPRITES_ORG:-$ORG}
FORWARD=${FORWARD:-8080}

here=$(cd "$(dirname "$0")" && pwd)
s() { sprite -o "$SPRITES_ORG" -s "$SPRITE" "$@"; }
# without --no-stdin, exec can return before the remote command finishes
sx() { s exec --no-stdin -- "$@"; }
# put SRC DEST MODE: upload over exec stdin and verify the checksum, because
# sprite file push cannot see the guest filesystem on some sprites
put() {
  local src=$1 dest=$2 mode=$3 want got
  want=$(sha256sum "$src" | cut -d' ' -f1)
  s exec --stdin -- sh -c "umask 077 && cat > '$dest.tmp'" < "$src"
  got=$(sx sha256sum "$dest.tmp" | tail -1 | cut -d' ' -f1)
  if [[ "$want" != "$got" ]]; then
    sx rm -f "$dest.tmp"
    echo "checksum mismatch pushing $src" >&2
    return 1
  fi
  sx sh -c "chmod $mode '$dest.tmp' && mv '$dest.tmp' '$dest'"
}

echo "==> building"
nix build "$here#wg6pn" --out-link "$here/result"
bin="$here/result/bin/wg6pn"

if [[ "${CREATE:-}" == 1 ]]; then
  echo "==> creating sprite $SPRITE"
  sprite -o "$SPRITES_ORG" create --skip-console "$SPRITE"
fi

# S2 sprites reach the gateway over their bridge at sprite.internal; S1
# sprites reach it over the network, identified by Fly-Src
if sx getent hosts sprite.internal >/dev/null 2>&1; then
  gateway=http://sprite.internal
else
  gateway=https://api.sprites.dev
fi
REGISTER_URL="$gateway/v1/gateway/custom_api/$CONNECTOR/graphql"
echo "==> registering through $REGISTER_URL"

echo "==> pushing binary"
sx mkdir -p /home/sprite/.local/bin /home/sprite/.config/wg6pn
put "$bin" /home/sprite/.local/bin/wg6pn 755

base="WG6PN_ORG=$ORG,WG6PN_REGISTER_URL=$REGISTER_URL,WG6PN_KEY_FILE=/home/sprite/.config/wg6pn/key"
[[ -n "${NETWORK:-}" ]] && base="$base,WG6PN_NETWORK=$NETWORK"
# sprite-env splits --env on commas
env="$base,WG6PN_FORWARD=${FORWARD//,/;}"

echo "==> validating config in the sprite"
s exec --no-stdin --env "$base" -- /home/sprite/.local/bin/wg6pn -check

echo "==> (re)creating service"
sx sprite-env services delete wg6pn >/dev/null 2>&1 || true
sx sprite-env services create wg6pn --cmd /home/sprite/.local/bin/wg6pn --env "$env" --duration 15s

echo "==> status (waiting up to 30s for the tunnel)"
for _ in $(seq 15); do
  if sx curl -sf http://127.0.0.1:9180/healthz >/dev/null 2>&1; then break; fi
  sleep 2
done
sx curl -s http://127.0.0.1:9180/
