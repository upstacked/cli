#!/usr/bin/env bash
# Acceptance run against the Network Lab on staging: build monitoring for a
# real SNMP device and a real API device the way an agent would, with nothing
# but `ups`, and prove the data reaches the portal with its labels.
#
# It is destructive by design (the lab asked for it): the hosts' items are
# deleted and a test template applied. Every exit re-applies the hosts' own
# templates and deletes what the run created.
#
#   UPS="ups --profile e2e" acceptance/staging.sh
#
# Needs jq. Takes ~10 minutes, most of it waiting for the agent to poll.
set -uo pipefail

UPS=${UPS:-ups}
INFRA=${INFRA:-8}
SNMP_HOST=${SNMP_HOST:-205}      # Border, Catalyst 9300
SNMP_RESTORE=${SNMP_RESTORE:-67} # its own template
SNMP_TAG=${SNMP_TAG:-SNMPv2}
SNMP_SCHEMA=${SNMP_SCHEMA:-2}    # Interfaces
API_HOST=${API_HOST:-204}        # BT-OSL-RACK, Meraki switch
API_RESTORE=${API_RESTORE:-66}
API_TAG=${API_TAG:-meraki}
API_SCHEMA=${API_SCHEMA:-3}
API_URL=${API_URL:-https://api.meraki.com/api/v1/devices/Q2HP-8D6J-L4TW/switch/ports/statuses}
DATA_WAIT=${DATA_WAIT:-420}      # seconds to wait for polled data

IF_NAME=1.3.6.1.2.1.31.1.1.1.1
IF_OPER=1.3.6.1.2.1.2.2.1.8
stamp=$(date +%Y%m%d-%H%M%S)
work=$(mktemp -d)
failures=0
created_templates=()
created_modules=()

u() { $UPS --infra "$INFRA" --yes "$@"; }
say() { printf '\n== %s\n' "$*"; }
pass() { printf '  PASS  %s\n' "$*"; }
fail() { printf '  FAIL  %s\n' "$*"; failures=$((failures + 1)); }
check() { # check "description" <jq filter that must be true> <file>
  if jq -e "$2" "$3" >/dev/null 2>&1; then pass "$1"; else fail "$1"; jq -c . "$3" 2>/dev/null | head -c 600; echo; fi
}

cleanup() {
  say "Restore"
  u monitoring template apply "$SNMP_RESTORE" --host "$SNMP_HOST" >/dev/null 2>&1 &&
    pass "template $SNMP_RESTORE re-applied to host $SNMP_HOST" || fail "re-apply template $SNMP_RESTORE to $SNMP_HOST"
  u monitoring template apply "$API_RESTORE" --host "$API_HOST" >/dev/null 2>&1 &&
    pass "template $API_RESTORE re-applied to host $API_HOST" || fail "re-apply template $API_RESTORE to $API_HOST"
  for t in "${created_templates[@]}"; do u monitoring template delete "$t" >/dev/null 2>&1; done
  for m in "${created_modules[@]}"; do u monitoring module delete "$m" >/dev/null 2>&1; done
  rm -rf "$work"
  say "Result"
  if [ "$failures" -eq 0 ]; then echo "  all checks passed"; else echo "  $failures check(s) failed"; fi
}
trap cleanup EXIT

# delete_items removes every item on a host, as the lab asked for.
delete_items() {
  for id in $(u monitoring item list --host "$1" --id-only 2>/dev/null); do
    u monitoring item delete "$id" >/dev/null 2>&1 || fail "delete item $id on host $1"
  done
  u monitoring item list --host "$1" --json >"$work/left.json" 2>/dev/null
  check "host $1 has no items left" '(.items // .) | length == 0' "$work/left.json"
}

# await_data waits until the host has published a row into the schema after
# $2 (a UTC timestamp) whose value-mapped field shows a label.
await_data() {
  local host=$1 since=$2 schema=$3 field=$4 deadline=$((SECONDS + DATA_WAIT))
  while [ $SECONDS -lt $deadline ]; do
    u monitoring schema data "$host" "$schema" --json >"$work/data.json" 2>/dev/null
    if jq -e --arg s "$since" --arg f "$field" \
      'map(select(.timestamp > $s and (.value_mapping[$f].mapped_value // "") != "")) | length > 0' \
      "$work/data.json" >/dev/null 2>&1; then
      pass "host $host published into schema $schema after the apply, and $field shows a label"
      jq -r --arg f "$field" '.[0:3][] | "        \(.name // .port__i_d): \($f) = \(.value_mapping[$f].mapped_value)"' "$work/data.json"
      return
    fi
    sleep 20
  done
  fail "no new data from host $host in schema $schema with a labelled $field within ${DATA_WAIT}s"
}

# ---------------------------------------------------------------------------
say "SNMP: host $SNMP_HOST"
delete_items "$SNMP_HOST"

snmp_cred=$(u credential list --json 2>/dev/null |
  jq -r --arg t "$SNMP_TAG" '[(.items // .)[] | select(.tag.name == $t and .scope != "system")][0].id')
u host walk "$SNMP_HOST" ifName ifOperStatus --credential "$snmp_cred" --json >"$work/walk.json" 2>"$work/walk.err"
check "walk returns interface rows" '.count > 0' "$work/walk.json"
check "walk sees ifOperStatus values the ifOperStatus value mapping covers" \
  '[.items[] | select(.name == "ifOperStatus") | .value] | any(. == "1" or . == "2")' "$work/walk.json"

u monitoring value-mapping show ifOperStatus --json >"$work/vm.json" 2>/dev/null
check "an existing value mapping for interface status is found to reuse" '.id != null' "$work/vm.json"

mod=$(u monitoring module create --name "Acceptance SNMP $stamp" --id-only 2>/dev/null) && created_modules+=("$mod")
tpl=$(u monitoring template create --name "Acceptance SNMP $stamp" --module "$mod" --id-only 2>/dev/null) && created_templates+=("$tpl")
[ -n "$mod" ] && [ -n "$tpl" ] && pass "module $mod and template $tpl created" || fail "create module and template"

item=$(u monitoring item create --template "$tpl" --module "$mod" --test-host "$SNMP_HOST" \
  --name "Interfaces" --data-source snmp --interval 1m --credential-tag "$SNMP_TAG" --timeout 10 \
  --params "{\"oid\":\"$IF_NAME,$IF_OPER\"}" --skip-test --id-only 2>"$work/item.err")
[ -n "$item" ] && pass "template item $item created" || { fail "create template item"; cat "$work/item.err"; }

u monitoring item show "$item" --json >"$work/item.json" 2>/dev/null
check "item has what the portal needs to open it: interval, credential, tag, timeout" \
  '.interval != null and .credential != null and (.credential_tag // "") != "" and .timeout == 10' "$work/item.json"
check "item is SNMP and polls one device" '.data_source.id == 2 and .host_specific_api_call == true' "$work/item.json"
check "item stores its OIDs as the string the portal splits" "(.parameters | fromjson | .oid) == \"$IF_NAME,$IF_OPER\"" "$work/item.json"

u monitoring item mapping create --item "$item" --schema "$SNMP_SCHEMA" \
  --field "port__i_d=item['\$.$IF_NAME'].key" \
  --field "name=item['\$.$IF_NAME'].value" \
  --field "oper__status=item['\$.$IF_OPER'].value" \
  --identifier port__i_d --multi-valued --value-mapping oper__status=ifOperStatus \
  --skip-test --json >"$work/map.json" 2>"$work/map.err"
check "multi-valued mapping created with the value mapping attached" \
  '.id != null and ([.field_mappings[] | select(.key == "oper__status") | .value_mapping] | .[0] != null)' "$work/map.json"

u monitoring item dry-run "$item" --host "$SNMP_HOST" --json >"$work/dry.json" 2>"$work/dry.err"
check "dry run on the device succeeds" '.status == "success"' "$work/dry.json"
check "dry run maps a row per interface with a status" \
  '(.data_points | length) > 1 and all(.data_points[]; .extra.value.oper__status != null)' "$work/dry.json"

u monitoring template update "$tpl" --publish >/dev/null 2>&1
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
u monitoring template apply "$tpl" --host "$SNMP_HOST" >/dev/null 2>"$work/apply.err" &&
  pass "template $tpl applied to host $SNMP_HOST" || { fail "apply template $tpl"; cat "$work/apply.err"; }
u monitoring item list --host "$SNMP_HOST" --json >"$work/applied.json" 2>/dev/null
check "host $SNMP_HOST now has exactly the test item" '(.items // .) as $i | ($i | length) == 1 and $i[0].name == "Interfaces"' "$work/applied.json"

await_data "$SNMP_HOST" "$since" "$SNMP_SCHEMA" oper__status

# ---------------------------------------------------------------------------
say "API: host $API_HOST"
delete_items "$API_HOST"

mod=$(u monitoring module create --name "Acceptance API $stamp" --id-only 2>/dev/null) && created_modules+=("$mod")
tpl=$(u monitoring template create --name "Acceptance API $stamp" --module "$mod" --id-only 2>/dev/null) && created_templates+=("$tpl")
[ -n "$mod" ] && [ -n "$tpl" ] && pass "module $mod and template $tpl created" || fail "create module and template"

item=$(u monitoring item create --template "$tpl" --module "$mod" --test-host "$API_HOST" \
  --name "Switch ports" --data-source api --interval 1m --credential-tag "$API_TAG" --timeout 30 \
  --params "{\"url\":\"$API_URL\"}" --skip-test --id-only 2>"$work/item.err")
[ -n "$item" ] && pass "template item $item created" || { fail "create template item"; cat "$work/item.err"; }

# One document per device or one for many is read off the response, as in the
# portal's scan step, not known up front.
u monitoring item dry-run "$item" --host "$API_HOST" --json >"$work/fetch.json" 2>/dev/null
check "the device answers the call" '.status == "success"' "$work/fetch.json"
printf '{"host_specific_api_call": true, "response_root_path": "$"}' >"$work/cfg.json"
u monitoring item update "$item" --from-file "$work/cfg.json" --skip-test >/dev/null 2>"$work/upd.err" ||
  { fail "save the proved config"; cat "$work/upd.err"; }

u monitoring item show "$item" --json >"$work/item.json" 2>/dev/null
check "item has what the portal needs to open it: interval, credential, tag, timeout, one-or-many" \
  '.interval != null and .credential != null and (.credential_tag // "") != "" and .timeout == 30 and .host_specific_api_call == true' "$work/item.json"

u monitoring item mapping create --item "$item" --schema "$API_SCHEMA" \
  --field "port__i_d=item['\$'].portId" \
  --field "name=\"PORT \" + item['\$'].portId" \
  --field "oper__status=item['\$'].status == \"Connected\"" \
  --field "duplex=item['\$'].duplex" \
  --identifier port__i_d --multi-valued \
  --value-mapping "oper__status=Meraki Interface Status" --value-mapping "duplex=Meraki Interface Duplex" \
  --skip-test --json >"$work/map.json" 2>"$work/map.err"
check "multi-valued mapping created with both value mappings attached" \
  '.id != null and ([.field_mappings[] | select(.value_mapping != null)] | length == 2)' "$work/map.json"

u monitoring item dry-run "$item" --host "$API_HOST" --json >"$work/dry.json" 2>"$work/dry.err"
check "dry run on the device succeeds" '.status == "success"' "$work/dry.json"
check "dry run maps a row per port with a status" \
  '(.data_points | length) > 1 and all(.data_points[]; .extra.value.oper__status != null)' "$work/dry.json"

u monitoring template update "$tpl" --publish >/dev/null 2>&1
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
u monitoring template apply "$tpl" --host "$API_HOST" >/dev/null 2>"$work/apply.err" &&
  pass "template $tpl applied to host $API_HOST" || { fail "apply template $tpl"; cat "$work/apply.err"; }

await_data "$API_HOST" "$since" "$API_SCHEMA" oper__status
