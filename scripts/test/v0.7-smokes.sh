#!/usr/bin/env bash
# v0.7 federation primitives smoke tests.
# Requires: fabric running, FABRIC_KEY exported, jq.
# The two-instance test is simulated: we POST coord with one origin_host,
# then call /v1/federation/coord/import with a different origin_host and
# verify the import path preserves the marker.

set -euo pipefail
FABRIC_URL=${FABRIC_URL:-http://localhost:8201}
KEY=${FABRIC_KEY:?FABRIC_KEY env var required}
H=(-H "Authorization: Bearer ${KEY}" -H "Content-Type: application/json")

pass() { printf "  \033[32mPASS\033[0m %s\n" "$1"; }
fail() { printf "  \033[31mFAIL\033[0m %s\n" "$1"; exit 1; }

stamp=$(date +%s)
peer_id="smoke-peer-${stamp}"
sender="smoke-fed-${stamp}"

echo "== v0.7 smoke test 1: peer register + list =="
curl -fsS "${H[@]}" -X POST "${FABRIC_URL}/v1/federation/peer" \
  -d "{\"id\":\"${peer_id}\",\"url\":\"http://127.0.0.1:9\",\"bearer_token\":\"unused\"}" >/dev/null
listed=$(curl -fsS "${H[@]}" "${FABRIC_URL}/v1/federation/peers" | jq -r ".peers[] | select(.id==\"${peer_id}\") | .id")
[[ "${listed}" == "${peer_id}" ]] || fail "peer not listed"
pass "peer ${peer_id} registered + listed"

echo "== v0.7 smoke test 2: bulk import preserves origin_host =="
body=$(cat <<EOF
[
  {"sender":"${sender}","recipient":"all","subject":"hello-from-peer","body":"federated msg 1","origin_host":"simulated-peer","ts":"$(date -u -Iseconds)"},
  {"sender":"${sender}","recipient":"all","subject":"hello-from-peer-2","body":"federated msg 2","origin_host":"simulated-peer","ts":"$(date -u -Iseconds)"}
]
EOF
)
imp=$(curl -fsS "${H[@]}" -X POST "${FABRIC_URL}/v1/federation/coord/import" -d "${body}")
imported=$(echo "${imp}" | jq -r .imported)
[[ "${imported}" == "2" ]] || fail "expected 2 imported, got ${imported}"

found=$(curl -fsS "${H[@]}" "${FABRIC_URL}/v1/coord/recent?limit=20" \
  | jq -r ".messages[] | select(.sender==\"${sender}\") | .origin_host" | sort -u)
[[ "${found}" == "simulated-peer" ]] || fail "origin_host not preserved: ${found}"
pass "import preserved origin_host=simulated-peer"

echo "== v0.7 smoke test 3: /coord/since/:id returns only local-origin messages =="
# Send a local coord message.
sent=$(curl -fsS "${H[@]}" -X POST "${FABRIC_URL}/v1/coord" \
  -d "{\"sender\":\"smoke-local-${stamp}\",\"recipient\":\"all\",\"subject\":\"local-marker-${stamp}\",\"body\":\"local body\"}")
local_id=$(echo "${sent}" | jq -r .id)
[[ "${local_id}" != "null" && -n "${local_id}" ]] || fail "could not send local coord"

# Pull everything since 0 — should include our local message and exclude simulated-peer rows.
pulled=$(curl -fsS "${H[@]}" "${FABRIC_URL}/v1/federation/coord/since/0")
has_local=$(echo "${pulled}" | jq -r ".messages[] | select(.id==${local_id}) | .id")
[[ "${has_local}" == "${local_id}" ]] || fail "local message ${local_id} missing from since/0"
has_remote=$(echo "${pulled}" | jq -r '.messages[] | select(.origin_host=="simulated-peer") | .id' | head -n1)
[[ -z "${has_remote}" ]] || fail "since/0 leaked a simulated-peer message (id=${has_remote})"
pass "since/0 returns local-origin only"

echo "all v0.7 smokes passed."
