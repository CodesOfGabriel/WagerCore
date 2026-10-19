#!/bin/sh
set -eu

client=${1:-internal-service}
issuer=${OIDC_ISSUER_URL:-http://localhost:8081/realms/wagercore}
secret=${CLIENT_SECRET:-${client}-local-secret}

curl --fail --silent --show-error "$issuer/protocol/openid-connect/token" \
    --data-urlencode grant_type=client_credentials \
    --data-urlencode "client_id=$client" \
    --data-urlencode "client_secret=$secret" |
    python3 -c 'import json, sys; print(json.load(sys.stdin)["access_token"])'
