#!/bin/bash
# Throwaway Fabric instance on a scratch database: reproduce the purge failure with the deployed binary,
# then prove the fix (migration 010 + patched purgeTenant) erases a tenant fully. Prints no secrets.
set -uo pipefail
DB=fabric_purge_test; PORT=8299
pw="$(grep -E '^TFS_DB_PASSWORD=' "$HOME/.kronaxis/env" | head -1 | cut -d= -f2-)"
P() { PGPASSWORD="$pw" psql -h 127.0.0.1 -U titan -v ON_ERROR_STOP=1 -qAt "$@"; }
SRC=$HOME/build/fabric-src/migrations
ADMIN="kxtest_$(openssl rand -hex 16)"
P -d postgres -c "DROP DATABASE IF EXISTS $DB" 2>/dev/null
P -d postgres -c "CREATE DATABASE $DB"
P -d $DB -c "CREATE EXTENSION IF NOT EXISTS vector" -c "CREATE EXTENSION IF NOT EXISTS pg_trgm" >/dev/null
P -d $DB -f $SRC/20260527_001_meta.sql >/dev/null
P -d $DB <<SQL >/dev/null
CREATE SCHEMA IF NOT EXISTS tenant_00000000;
INSERT INTO kronaxis_meta.tenants (id, display_alias, schema_name, tenant_type, status)
  VALUES ('00000000-0000-7000-8000-000000000000','test_zero','tenant_00000000','platform','active');
INSERT INTO kronaxis_meta.tenant_keys (tenant_id, key_hash, key_prefix, scope)
  VALUES ('00000000-0000-7000-8000-000000000000', encode(sha256(convert_to('$ADMIN','UTF8')),'hex'), substr('$ADMIN',1,8), 'admin');
SQL
run_fabric() {  # binary logfile
  FABRIC_KEY="$ADMIN" FABRIC_LISTEN=127.0.0.1:$PORT PGPASSWORD="$pw" FABRIC_PG_DSN="postgres://titan@127.0.0.1:5432/$DB" \
    MXBAI_RERANK_URL=http://127.0.0.1:8204 RERANK_DEFAULT_TRUE=false "$1" >"$2" 2>&1 &
  echo $! > /tmp/fabric_purge_test.pid
  for _ in $(seq 1 40); do curl -s -o /dev/null --max-time 2 http://127.0.0.1:$PORT/v1/health && return 0; sleep 0.5; done
  echo "fabric did not start; log tail:"; tail -5 "$2"; return 1
}
stop_fabric() { kill "$(cat /tmp/fabric_purge_test.pid)" 2>/dev/null; sleep 1; }
cycle() {  # label -> creates tenant, writes a memo with the tenant key, soft deletes, purges
  local label="$1" resp tid tkey code body
  resp=$(curl -s -X POST -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
    -d "{\"display_alias\":\"client_$label\",\"tenant_type\":\"customer\"}" http://127.0.0.1:$PORT/v1/tenant)
  tid=$(printf '%s' "$resp" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("tenant_id",""))')
  tkey=$(printf '%s' "$resp" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("bearer_key",""))')
  [ -n "$tid" ] || { echo "[$label] tenant create failed: $(printf '%s' "$resp" | head -c 200)"; return 1; }
  code=$(curl -s -o /tmp/fpt_memo.json -w '%{http_code}' -X POST -H "Authorization: Bearer $tkey" -H 'Content-Type: application/json' \
    -d '{"title":"purge test memo","content":"Synthetic test content for the purge test. No personal data.","type":"project"}' http://127.0.0.1:$PORT/v1/memo)
  local schema; schema=$(P -d $DB -c "select schema_name from kronaxis_meta.tenants where id='$tid'")
  echo "[$label] tenant $tid schema $schema memo write http $code; memos in schema: $(P -d $DB -c "select count(*) from $schema.memos")"
  curl -s -o /dev/null -X DELETE -H "Authorization: Bearer $ADMIN" http://127.0.0.1:$PORT/v1/tenant/$tid
  local sd; sd=$(P -d $DB -c "select schema_name from kronaxis_meta.tenants where id='$tid'")
  body=$(curl -s -w ' http=%{http_code}' -X POST -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
    -d "{\"confirm\":\"$tid\"}" http://127.0.0.1:$PORT/v1/tenant/$tid/purge)
  echo "[$label] purge: $(printf '%s' "$body" | head -c 300)"
  echo "[$label] after: tenants=$(P -d $DB -c "select count(*) from kronaxis_meta.tenants where id='$tid'") keys=$(P -d $DB -c "select count(*) from kronaxis_meta.tenant_keys where tenant_id='$tid'") schema $sd present=$(P -d $DB -c "select count(*) from pg_namespace where nspname='$sd'")"
  echo "[$label] audit rows for tenant: $(P -d $DB -c "select string_agg(action || ':' || coalesce(detail::text,''), ' | ' order by id) from kronaxis_meta.audit_log where target_tenant_id='$tid'")"
}
echo "== 1. deployed binary, migrations 001 only (reproduce)"
run_fabric /opt/kronaxis/behavioural-os/bin/fabric /tmp/fabric_purge_test_old.log && cycle old; stop_fabric
echo "== 2. migration 010 + patched binary"
P -d $DB -f $SRC/20260930_010_audit_fk_set_null.sql >/dev/null && echo "010 applied"
P -d $DB -c "select conname, confdeltype from pg_constraint where conrelid='kronaxis_meta.audit_log'::regclass and contype='f' order by 1"
run_fabric $HOME/build/fabric-new /tmp/fabric_purge_test_new.log && cycle new; stop_fabric
P -d postgres -c "DROP DATABASE $DB" && echo "scratch db dropped"
