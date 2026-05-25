#!/usr/bin/env bash
# v0.5 code graph smoke tests.
# Requires: fabric running, FABRIC_KEY exported, jq.

set -euo pipefail
FABRIC_URL=${FABRIC_URL:-http://localhost:8201}
KEY=${FABRIC_KEY:?FABRIC_KEY env var required}
H=(-H "Authorization: Bearer ${KEY}" -H "Content-Type: application/json")

pass() { printf "  \033[32mPASS\033[0m %s\n" "$1"; }
fail() { printf "  \033[31mFAIL\033[0m %s\n" "$1"; exit 1; }

stamp=$(date +%s)
repo="smoke-repo-${stamp}"

echo "== v0.5 smoke test 1: upsert symbol + name search =="
upsert1=$(curl -fsS "${H[@]}" -X POST "${FABRIC_URL}/v1/symbol" -d "$(cat <<EOF
{"repo":"${repo}","file_path":"cmd/fabric/main.go","symbol_name":"handleSearch","symbol_kind":"function","language":"go","line_start":376,"line_end":477,"signature":"func (s *server) handleSearch(w http.ResponseWriter, r *http.Request)","docstring":"Hybrid memo search endpoint."}
EOF
)")
sym_id=$(echo "${upsert1}" | jq -r .id)
[[ "${sym_id}" != "null" && -n "${sym_id}" ]] || fail "symbol upsert returned no id: ${upsert1}"

search=$(curl -fsS "${H[@]}" -X POST "${FABRIC_URL}/v1/symbol/search" \
  -d "{\"query\":\"handleSearch\",\"repo\":\"${repo}\",\"top_k\":5,\"mode\":\"hybrid\"}")
top_name=$(echo "${search}" | jq -r '.results[0].symbol_name')
top_score=$(echo "${search}" | jq -r '.results[0].score')
[[ "${top_name}" == "handleSearch" ]] || fail "expected handleSearch first, got ${top_name}"
awk_ok=$(awk -v s="${top_score}" 'BEGIN { exit (s >= 0.9) ? 0 : 1 }' && echo y || echo n)
[[ "${awk_ok}" == "y" ]] || fail "score ${top_score} below 0.9"
pass "search returns handleSearch with score ${top_score}"

echo "== v0.5 smoke test 2: edges + callers =="
# Add a caller symbol pointing at handleSearch.
upsert2=$(curl -fsS "${H[@]}" -X POST "${FABRIC_URL}/v1/symbol" -d "$(cat <<EOF
{"repo":"${repo}","file_path":"cmd/fabric/main.go","symbol_name":"routes","symbol_kind":"function","language":"go","line_start":600,"line_end":700,"signature":"func (s *server) routes() http.Handler","docstring":"HTTP route registration; calls handleSearch."}
EOF
)")
routes_id=$(echo "${upsert2}" | jq -r .id)
curl -fsS "${H[@]}" -X POST "${FABRIC_URL}/v1/symbol/edge" \
  -d "{\"src_id\":${routes_id},\"dst_id\":${sym_id},\"edge_kind\":\"calls\"}" >/dev/null

callers=$(curl -fsS "${H[@]}" "${FABRIC_URL}/v1/symbol/${sym_id}/callers")
caller_name=$(echo "${callers}" | jq -r '.results[0].symbol_name')
[[ "${caller_name}" == "routes" ]] || fail "expected routes as caller, got ${caller_name}"
pass "callers returns routes()"

echo "== v0.5 smoke test 3: reindex clears symbols for file =="
clr=$(curl -fsS "${H[@]}" -X POST "${FABRIC_URL}/v1/symbol/reindex" \
  -d "{\"repo\":\"${repo}\",\"file_path\":\"cmd/fabric/main.go\"}")
cleared=$(echo "${clr}" | jq -r .cleared)
[[ "${cleared}" -ge 2 ]] || fail "expected >=2 cleared, got ${cleared}"
# After clearing, name search returns nothing for this repo.
post=$(curl -fsS "${H[@]}" -X POST "${FABRIC_URL}/v1/symbol/search" \
  -d "{\"query\":\"handleSearch\",\"repo\":\"${repo}\",\"top_k\":5,\"mode\":\"name\"}")
count=$(echo "${post}" | jq -r '.results | length')
[[ "${count}" == "0" ]] || fail "expected 0 results after reindex, got ${count}"
pass "reindex soft-deleted ${cleared} symbols"

echo "all v0.5 smokes passed."
