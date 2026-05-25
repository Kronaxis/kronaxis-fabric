#!/usr/bin/env bash
# v0.8 router learning smoke tests.
# Requires: fabric running, FABRIC_KEY exported, jq.

set -euo pipefail
FABRIC_URL=${FABRIC_URL:-http://localhost:8201}
KEY=${FABRIC_KEY:?FABRIC_KEY env var required}
H=(-H "Authorization: Bearer ${KEY}" -H "Content-Type: application/json")

pass() { printf "  \033[32mPASS\033[0m %s\n" "$1"; }
fail() { printf "  \033[31mFAIL\033[0m %s\n" "$1"; exit 1; }

stamp=$(date +%s)
cheap="smoke-cheap-${stamp}"
pricey="smoke-pricey-${stamp}"
cat="extraction-smoke-${stamp}"

echo "== v0.8 smoke test 1: 20 observations across 2 models =="
# 10 cheap successes
for i in $(seq 1 10); do
  curl -fsS "${H[@]}" -X POST "${FABRIC_URL}/v1/router/observation" \
    -d "{\"request_hash\":\"r${i}\",\"task_category\":\"${cat}\",\"model_id\":\"${cheap}\",\"cost_usd\":0.001,\"latency_ms\":200,\"outcome\":\"success\"}" >/dev/null
done
# 10 pricey: 9 success 1 fail
for i in $(seq 1 9); do
  curl -fsS "${H[@]}" -X POST "${FABRIC_URL}/v1/router/observation" \
    -d "{\"request_hash\":\"r${i}\",\"task_category\":\"${cat}\",\"model_id\":\"${pricey}\",\"cost_usd\":0.05,\"latency_ms\":1500,\"outcome\":\"success\"}" >/dev/null
done
curl -fsS "${H[@]}" -X POST "${FABRIC_URL}/v1/router/observation" \
  -d "{\"request_hash\":\"r10\",\"task_category\":\"${cat}\",\"model_id\":\"${pricey}\",\"cost_usd\":0.05,\"latency_ms\":1500,\"outcome\":\"failed\"}" >/dev/null
pass "20 observations recorded"

echo "== v0.8 smoke test 2: cheaper model wins when both meet quality bar =="
rec=$(curl -fsS "${H[@]}" "${FABRIC_URL}/v1/router/recommend?category=${cat}")
top=$(echo "${rec}" | jq -r '.recommendations[0].model_id')
[[ "${top}" == "${cheap}" ]] || fail "expected ${cheap} to win, got ${top}: ${rec}"
pass "cheap model wins (${top})"

echo "== v0.8 smoke test 3: success_rate + sample_size correct =="
cheap_rate=$(echo "${rec}" | jq -r ".recommendations[] | select(.model_id==\"${cheap}\") | .success_rate")
pricey_rate=$(echo "${rec}" | jq -r ".recommendations[] | select(.model_id==\"${pricey}\") | .success_rate")
cheap_n=$(echo "${rec}" | jq -r ".recommendations[] | select(.model_id==\"${cheap}\") | .sample_size")
pricey_n=$(echo "${rec}" | jq -r ".recommendations[] | select(.model_id==\"${pricey}\") | .sample_size")
[[ "${cheap_rate}" == "1" ]] || fail "cheap success_rate ${cheap_rate}, expected 1"
[[ "${pricey_rate}" == "0.9" ]] || fail "pricey success_rate ${pricey_rate}, expected 0.9"
[[ "${cheap_n}" == "10" ]] || fail "cheap sample_size ${cheap_n}, expected 10"
[[ "${pricey_n}" == "10" ]] || fail "pricey sample_size ${pricey_n}, expected 10"
pass "rates + counts correct"

echo "all v0.8 smokes passed."
