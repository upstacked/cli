---
name: upstacked
description: Operate Upstacked infrastructure via the `ups` CLI — devices, monitoring, MIBs and OID lookup, credentials, IPAM, changes, runbooks, tickets, and infrastructure-as-code. Use whenever the task involves the `ups` command, Upstacked hosts/infrastructures/monitoring, building SNMP or API checks for a device, or a user asks to inspect, change, or document network infrastructure managed by Upstacked.
---

# Upstacked CLI

`ups` manages monitored network infrastructure: real devices, real alerts, real people
who get paged. Most commands here touch production. This document is mostly about **why**
the workflows are shaped the way they are — the `how` is in `ups <command> --help`, which
is always more current than this file.

## You are probably running without a terminal

This trips agents more than anything else here, so deal with it first.

`ups` never prompts when stdin is not a terminal. It fails instead, with exit code 2 and a
message naming the flag you needed. That is deliberate — a prompt nobody can answer is a
hang — but it means:

- **Mutating commands need `--yes`.** Delete, apply, maintenance close, discovery start and
  runbook run all confirm first. Without a terminal they fail rather than proceeding.
- **`ups login` cannot prompt for a password.** Use `--password-stdin`, or set
  `UPSTACKED_TOKEN` in the environment and skip login entirely.
- **`ups init` and `ups context set` want to show a picker.** Pass `--non-interactive` and
  the explicit ids, or they fail.

`--yes` suppresses the *prompt*, not the *safety*. It does not permit deletions in
`ups apply` — that still needs `--allow-delete` — and it does not lower any other guard.

Run `ups doctor` first when anything looks wrong. It checks the local setup only, reports
every problem at once rather than stopping at the first, and exits non-zero if any check
fails.

## Mental model

An **infrastructure** is the top-level scope, and it is what `--infra` selects. It belongs
to a **customer**. Almost everything else hangs off an infrastructure:

```
customer
└── infrastructure
    ├── host (a device)          ── asset (procurement/ownership record, optional link)
    │   ├── monitoring item      ── monitoring module (what to check)
    │   │                        ── credential (how to authenticate)
    │   └── host_link            (topology edge to another host)
    ├── credential               (snmpv2 | snmpv3 | api | device | oauth2 | vendor-specific)
    ├── subnet ── ip_address     (IPAM)
    ├── runbook ── elements      (automation, executed against the infrastructure)
    ├── change ── change_log     (what was done, and the field-level audit trail)
    └── monitoring_event         (an alert; becomes an incident)
```

Two things follow from this shape, and both matter:

**Everything is infra-scoped, so ambiguity is dangerous.** The same hostname —
`core-sw-01`, `fw-01` — exists in many customers' infrastructures. A command that resolves
a name without a pinned infrastructure can act on the wrong customer's device. That is not
a bad command, it is an incident. Always confirm which infrastructure is active
(`ups context show`) before any write. If a name resolves to more than one host, `ups`
will refuse and list the candidates — do not pick one arbitrarily, ask the user.

**A host is not an asset.** A host is the thing that gets monitored. An asset is the
procurement/ownership record. They can be linked, but they are separate objects with
separate lifecycles. Deleting one does not delete the other. Do not treat them as
interchangeable.

## Rules that prevent damage

These exist because of specific failure modes. Follow them even when they seem like extra steps.

### Silent loss of monitoring coverage is the worst outcome

When a monitoring item is deleted or broken, **nothing pages anyone**. It just stops
watching. The failure is invisible until the day it was supposed to catch something and
didn't. Every other failure in this system announces itself; this one does not.

So: before anything that could remove or alter monitoring — `ups apply`, bulk update,
bulk delete — run the diff and read it. Coverage removal is the line item to look for.

### A monitoring item is not trustworthy until it has returned data

A misconfigured item does not error. It returns nothing, or it returns the wrong field,
and you get either silence or false alerts. A config can be structurally valid and still
collect nothing, because whether it works depends on the shape of what the device returns
— which varies by device, firmware and API version, so no amount of static checking
settles it. Running it is the only feedback loop that exists.

**A dry run is that feedback loop.** It executes the real monitoring pipeline once —
fetch, host mapping, schema mapping, alert evaluation — with the writing ends replaced by
ones that record instead of publish. Nothing reaches Elasticsearch, no alert is raised, and
what comes back is the data the config *would* have produced, in the shape real monitoring
data takes. This is how you confirm a config you generated actually works before you treat
it as done.

```
ups monitoring item create --host <id> --name "CPU" --module <id>   # creates, then dry-runs
ups monitoring item dry-run <item-id>                               # re-check an existing item
ups monitoring item dry-run <item-id> --host <host-id>              # against a specific device
ups monitoring item dry-run <item-id> --from-file config.json       # with unsaved overrides
ups monitoring item dry-run show <run-id>                           # read back a queued run
```

`create` therefore dry-runs the item it just made. Read the result. If the dry run fails,
the item exists but is collecting nothing — fix it or remove it, and tell the user; do not
leave it and move on. A dry run that would publish nothing exits non-zero, whether it
failed outright or every stage passed and produced no data points.

**You can check a config before saving it.** `--from-file` takes a JSON object of overrides
— `parameters`, `mapping_rules`, `response_root_path`, `host_specific_api_call`, `timeout`,
`schema_mapping`, `host` — applied in memory and never written. So the loop is: dry-run the
candidate config, read the trace, adjust, dry-run again, and only then save. A field the
API would not honour is refused rather than silently dropped, because a dropped override
reads back as "that was checked" when nothing checked it.

**Read the trace, not just the verdict.** It reports each stage separately, so a failure
says *where* it failed rather than just that it did:

| Stage | A failure here means |
|---|---|
| fetch | the device or API did not answer, or answered with an error |
| host mapping | the response came back but nothing in it matched this host |
| schema mapping | the data was found but the field expressions did not resolve |

Host mapping is the one that misleads. On a failure `ups` prints `candidate_identifiers` —
what each candidate in the response actually rendered to — next to the identifier
expression and the value it was matched against. That is usually the whole answer.

**Only `api_data`, `snmpstd` and `icmp` have mapping stages to preview.** Meraki, DNAC,
Viptela, Webex, Cybervision and the legacy `snmp` worker are refused with a message saying
so. For those, `ups monitoring item test` is the check that still applies.

**The dry run reads `data_source`, not the legacy `action_type`.** An item typed by a
legacy action (`--data-source snmp:walk`, an id) had no `data_source` on older servers and
is refused with "no data source that can be dry run". Set it by name —
`ups monitoring item update <id> --data-source snmp` (or `api`, `icmp`) — which writes
`data_source` directly. If it is still refused, fall back to `ups monitoring item test`,
say plainly that the stronger check could not run, and do not report the item as verified.

**A dry run is queued, not synchronous, and handed to an agent exactly once.** It runs on
the customer's monitoring agent, which polls for work every few seconds, so expect a wait.
`ups` polls for the outcome rather than reporting the dispatch as a success. A run nobody
reports on is failed at two minutes — but only when it is read, so poll rather than assume
pending means running. Do not retry by reading the same run again; that never re-dispatches
it. Queue a new run.

**Results are capped** at 100 data points and 200k characters of raw response. When a
result was capped `ups` says so on stderr. Never pass a capped preview on as complete.

`--skip-test` skips the check entirely. Using it means nobody has confirmed the check works.

Every monitoring item belongs to an organization, and the API refuses a create without one.
`ups` fills it in when you belong to exactly one; when you belong to several it stops and
asks for `--org` rather than picking. That is not a guess worth making — an item filed under
the wrong organization is invisible to the people who should see it.

### `test` answers a weaker question than `dry-run`

```
ups monitoring item test <item-id>      # fetch only: did the device answer?
ups monitoring item results <item-id>   # the most recent test result
```

`test` still works and is still worth having, but it stops at the raw response. It never
runs host or schema mapping, so it cannot tell you whether a config produces data — only
whether something answered. It also re-implements its own SNMP/HTTP/ICMP calls and falls
through to a bare HTTP GET for anything it does not recognise, so a passing test is weaker
evidence than it looks.

Reach for `dry-run` by default. Reach for `test` when the data source has no mapping stages
to preview and the dry run refuses it. `create` does this automatically: it dry-runs, falls
back to a test when the source is unsupported, and says which check actually ran.

### `CONFIG` on an item means a dry run confirmed it

`monitoring_item_config_status` is derived from dry runs, and `ups monitoring item list`
and `show` display it. It reads `COMPLETE` only while a successful dry run still matches
the item's current config fingerprint — which covers the config fields, the schema mappings
*and* the device. Editing the config or repointing it at another host invalidates that and
flips the item back to `INCOMPLETE`.

Saving is never blocked: base fields save freely and an incomplete item is a legal state,
not an error. So `INCOMPLETE` is not a warning the platform will act on — it means nobody
has confirmed this item collects anything, and per the coverage rule above, nothing will
tell you later.

### Applying a monitoring template replaces a host's monitoring

A template is the set of checks a kind of device gets. Applying one is **not a merge**:
every monitoring item on the host is deleted first, then the template's items are created
in their place. Anything added to that host by hand is gone, and per the coverage rule
above, nobody is told.

```
ups monitoring template list
ups monitoring template show <id>
ups monitoring template items <id>        # what would actually be checked
ups monitoring template preflight <id>    # credentials the infrastructure is missing
ups monitoring template apply <id> --host <h1,h2>
```

`apply` lists the items it will remove and confirms before writing, and preflights first
unless `--skip-preflight` is given. Read that list rather than passing `--yes` past it: it
is the only place the removal is ever visible. The items it creates are unchecked; dry-run
them on the first host before applying to the rest.

Templates are authored the other way round from items:

```
ups monitoring template create --name "Cisco IOS switch" --module <m1,m2>
ups monitoring item create --template <id> --module <m1> --name "CPU"
ups monitoring template update <id> --publish
```

Two things about this shape trip people up, and both are checked by the CLI:

- **A template holds modules, not items.** An item belongs to a template because its
  module is in that template's module set. Creating an item with a module outside the set
  produces an item that is never applied, so `item create --template` refuses it and tells
  you to `--add-module` first.
- **Templates share items through modules.** An item added under a module that another
  template also holds joins that template too. The CLI warns and asks; do not wave it
  through without telling the user which other templates change.

A host-less template item has no device to poll until it is applied, so the usual
create-then-dry-run loop has nothing to run against. Give it one with `--test-host
<host-id>` on `item create` or `item update`: every dry run after a change then runs
against that device, without applying the template. Without a test host, apply the
template to one host and dry-run there before rolling it out to the rest.

### Preflight a runbook before running it

```
ups runbook preflight <runbook>
ups runbook run <runbook> --yes
```

Runbooks execute against live network devices. A run that fails halfway because a
credential was missing can leave a device **partially configured** — worse than not having
run at all. The preflight is cheap. Partial execution is not.

`run` preflights on its own and refuses to start when credentials are missing.
`--skip-preflight` overrides that; do not reach for it to get past the error.

### Validate before importing

```
ups asset import validate <file.csv>   # errors surface here
ups asset import apply <file.csv> --yes
```

The API deliberately separates validation from import. Treat that split as a signal: the
import is not trivially reversible.

### Open a maintenance window before working on monitored devices

Work on a live device generates alerts. Those alerts page humans, and they also pollute
availability reporting — self-inflicted downtime shows up in customer-facing numbers
unless it is inside a declared window.

```
ups maintenance create --hosts <h1,h2> --duration 2h --reason "..."
```

`ups change create --hosts ... --window 2h` opens both at once, which is usually what
planned work wants.

### Never put a secret in argv

```
printf '%s' "$PW" | ups credential create snmpv3 --name core --username admin --secret-stdin
```

Not as a flag value. `argv` is visible to `ps`, lands in shell history, and in CI lands in
build logs — and these credentials authenticate to live network equipment. Every command
that takes a secret offers `--secret-stdin` or `--secret-file`; `ups login` offers
`--password-stdin` and `--password-file`.

Never echo a secret back to the user, into a file, or into a commit.

## Building monitoring for a device

Everything above is about not breaking monitoring that already exists. This is
how to build a check that works in the first place.

A check is three decisions, in this order:

1. **Which data schemas the device must populate.** A schema is the contract
   everything downstream reads — graphs, alert rules, the host's metric pages.
   A check that collects real data into no schema is invisible to all of them.
2. **Where the values come from.** For SNMP that is OIDs out of the vendor's
   MIB. For an API it is JSON paths into a documented response.
3. **How the checks group.** Items belong to modules, modules belong to
   templates, and a template is what a whole class of device gets.

Only the second differs between SNMP and an API. The rest is the same work.

### 1. Decide which schemas the device must fill

```
ups monitoring schema list               # the catalogue
ups monitoring schema show <id>          # its keys, and which one identifies a row
ups monitoring schema host <host-id>     # what a device already publishes
```

Start with the last one. A device of the same type that is already monitored
properly has the answer, and copying a working config beats deriving one:
`ups monitoring item list --host <id>` then
`ups monitoring item mapping list --item <id>` reads the whole thing out, paths
included.

`schema show` marks one field as the identifier. That is the field that tells
rows apart — the interface, the sensor, the disk — and it is the one a
multi-valued check cannot do without.

When nothing in the catalogue fits, define one — but look first, because a
schema is shared by every check that maps into it and is read by name from
graphs and alert rules. A near-duplicate splits a device's history across two
names that nothing joins back up.

```
ups monitoring schema create --name interface --field if_name:STRING --identifier if_name
ups monitoring schema add-key <schema-id> --field errors:INTEGER
```

Field types are `STRING`, `INTEGER`, `FLOAT` and `BOOLEAN`. Adding a key is
safe: existing mappings keep filling what they already fill. There is no remove
— the API's delete endpoint takes no argument saying which key to drop, so
`ups` will not guess at it.

### 2a. SNMP: walk the MIB, then the device

The OIDs worth polling are almost never the scalars in SNMPv2-MIB; they are
vendor tables, and no amount of guessing produces them. So the MIB is the
reference, and `ups` keeps one locally:

```
ups mib sync                              # clone and index; ~450 MB on disk, once
ups mib search cpmCPUTotal                # find an object by name
ups mib search "input errors" --describe  # ...or by what the vendor says it measures
ups mib walk ifXTable                     # list a subtree, in OID order
ups mib show ifHCInOctets                 # numeric OID, syntax, description
```

`sync` clones LibreNMS's collection and Cisco's, then resolves every object's
numeric OID by walking parent chains across all cached MIBs at once. It is the
only step that touches the network; search, walk and show are offline and do
not go near a device. Say what it will cost before running it unprompted: the
cache lands around 450 MB under `ups mib path`, and a minute of clone time.
`ups mib status` says whether it is already there. Add a vendor's own
collection with `ups mib sync --repo <git-url>`.

**Walk the table before choosing OIDs.** The walk is what catches the three
mistakes that produce a check which looks healthy and is wrong:

- taking `ifInOctets` when the device populates `ifHCInOctets` — a Counter32
  wraps on a gigabit link in under a minute, and the graph just looks noisy;
- taking a scalar when the device only populates the table;
- taking the table's index column instead of the counter beside it.

`show` prints an empty OID when the MIB defining that object's parent is not
cached. That is reported rather than guessed, because a guessed OID polls a
different object and the check still comes back green. Sync more repositories
instead of filling it in.

The index is TSV — name, oid, kind, syntax, access, module, file, description —
at the directory `ups mib path` prints, so grep answers whatever the flags do
not.

A MIB says what an object is. It **does not say the device implements it**.
Walk the device to find out, before creating anything:

```
ups host walk <host-id> ifName ifHCInOctets --credential <snmp-credential-id>
```

This asks the device itself, through the infrastructure's monitoring agent:
which rows exist, what they are indexed by, and what the values look like. Names
resolve through the MIB cache; numeric OIDs are sent as given. Nothing is saved
or published. Walk the columns you mean to map, not whole tables — a large
switch answers a table walk with thousands of rows. A walk the device refuses
fails the command; an object it does not implement comes back as no rows.

It ends with what the item and its mapping take, which is the part that is easy
to get wrong by hand:

- **The item's parameter is `oid`: the column OIDs as one comma-separated
  string.** The SNMP pipeline reads that key only; `oids`, or a map of names,
  saves fine and polls nothing. The portal splits the string when it opens the
  item, so write the string form (servers normalise a list, older ones do not).
- **A table is a `--multi-valued` mapping.** Each row reads a column as
  `item['$.<column-oid>'].value`, and `.key` is the row's index. The row columns
  the engine joins on (`selected_json_path`) are derived from the `--field`
  paths.
- **`--identifier` is a schema key, not a path** — one of the keys you gave
  `--field`, usually the index or the name. The engine reads it from the
  mapped row by key; a path finds nothing, and every row's alerts then share
  one identity. `ups` refuses anything that is not one of the mapping's keys.

A dry run of the item then settles the rest.

### 2b. API: read the documentation, then one real response

Establish two things from the docs before writing any paths: which call returns
the data, and whether that call answers for one host or returns one document
covering all of them. The second decides `host_specific_api_call`. Get it wrong
and you have either N times the request volume, or a response the host mapping
has no way to split.

Then fetch one real response and write the paths against that, not against the
documentation. The two disagree often enough — a field renamed, a list wrapped
in an envelope, a version that never shipped — that paths derived from docs
alone are a guess. `ups monitoring item test <id>` returns the raw body, and
this is the one job it is better at than a dry run.

### 3. Prove the config before saving it

```
ups monitoring action list
ups monitoring interval list
ups monitoring item create --host 12 --name "Interface counters" --module 3 --data-source snmp \
  --interval 5m --credential-tag SNMPv2 --test-host 12
ups monitoring item dry-run <item-id> --from-file config.json
ups monitoring item update <item-id> --from-file config.json
```

**`--data-source` is required and has no sensible default.** It is what decides
whether the check speaks SNMP, HTTP or ICMP; an item without one polls nothing,
so `create` refuses rather than making a check that can never run.

**`--interval` is required for the same reason.** An item with no interval is
never scheduled, and nothing reports it. It is one of a fixed set of durations
(`ups monitoring interval list`: 1m, 5m, 15m, 30m, 1h, 2h, 1d); anything else is
refused rather than rounded. `--timeout` takes seconds.

**Give an item its credential by tag.** `--credential-tag SNMPv2` sets the tag
and picks this infrastructure's credential with it; `--credential <id>` sets
the credential and its tag. The portal needs both to open the item, and a
template uses the tag to find the right credential on each infrastructure it
is applied to.

Prefer `api`, `snmp` or `icmp`. Those are data sources, the current model: they
are written as `data_source`, the server derives the legacy `action_type` from
them, and they are the only sources a dry run can execute. `snmp` is the
item-level SNMP pipeline, which walks.

Anything else is a legacy action from `ups monitoring action list`, written as
`action_type`: an id, a `type:name` pair, or a bare type that offers only one
action. Legacy `snmp:get` and `snmp:walk` are not interchangeable: `get` reads
named scalars, `walk` enumerates a table. A bare type that matches two is refused
with both named; do not pick one to get past the error. A number is always a legacy action id, never a data
source id — the two id spaces overlap, so `1` means `meraki:host_status`, not API.

The dry run reports the source under a third set of worker names (`snmpstd` for
the current SNMP source, `snmp` for the legacy one). Read the trace rather than
assuming which is meant.

`create` makes the item and dry-runs it. `dry-run --from-file` then applies a
candidate config in memory — `parameters`, `response_root_path`,
`mapping_rules`, `host_specific_api_call`, `timeout`, `schema_mapping`, `host`
— and reports what it would collect without saving any of it. Iterate there.

`item update --from-file` takes the same file, so the config that was proved is
the config that gets stored, with nothing retyped in between. `item create`
accepts it too, which is how a config proved on one device is copied onto the
next.

Every edit invalidates the dry run that confirmed the item, so `update`
dry-runs again afterwards. That is not ceremony: the config status is derived
from dry runs, and nothing else will ever tell you the new value stopped
resolving.

### 4. Map the response onto the schema

An item says how to reach the data. It does not say what the data means. An
item with no schema mapping **fetches happily and publishes nothing** — the
failure mode this whole section is arranged to avoid.

```
ups monitoring item mapping create --item <item-id> --schema 7 --field in_octets=$.ifHCInOctets
ups monitoring item mapping list --item <item-id>
ups monitoring item mapping update <mapping-id> --identifier if_name --multi-valued
```

`--field key=path` puts the schema key on the left and the JSON path on the
right. Paths are evaluated after `response_root_path` has been applied, so
write them relative to that root and not to the whole body. A path may end in
a Jinja-style filter, as the portal's own mappings do: `item['$.1.3.6…5.1.3'].value | int`.

`--identifier` must be the key the **schema** marks as its identifier
(`ups monitoring schema show <id>`), not merely one of the keys you mapped.
Every item publishing into a schema identifies its rows the same way, and `ups`
refuses anything else.

`--multi-valued` is for a response carrying many rows, and it needs
`--identifier`: the schema key whose value tells the rows apart. Without one every interface
on the switch **collapses onto one series**, which reads as working monitoring
and is not.

`update --field` merges by key. Changing one path leaves the other fields
alone, and keeps the `filter_rules` and `value_mapping` on the field being
changed, because dropping them unasked would be a silent change of meaning.
`--remove-field` and `--replace-fields` do remove coverage, and both confirm
first.

For the parts flags cannot express — filter rules and alert rule config —
`--from-file` takes the whole request body, and `dry-run --from-file` will
preview a `schema_mapping` before any of it is written.

### 4b. Make the values readable: value mappings

A field publishes what the device sent: `1`, `true`, `1000000000`. A value
mapping is what shows that as **UP** in green on the host page. Map every field
whose raw value a person cannot read at a glance — status codes, booleans,
enumerations like duplex, speeds in bits per second.

```
ups monitoring value-mapping list                      # reuse first
ups monitoring value-mapping show ifOperStatus
ups monitoring value-mapping create --name ifOperStatus \
  --rule 1=UP@green --rule 2=DOWN@red --rule 3=TESTING@yellow
ups monitoring item mapping update <mapping-id> --value-mapping oper_status=ifOperStatus
```

**Reuse before creating.** The organization usually has one already for the
common cases (interface status, duplex, speed), the portal lists them by name,
and `create` refuses a duplicate name. Pick the existing mapping whose rules
match the raw values the dry run or `host walk` showed — `show` prints them —
and create one only when none does.

Rules are tried in order and the first match wins: `VALUE=LABEL[@COLOUR]` for
equality (compared as text, any case), `range:LOW-HIGH=`, `gte:N=`, `lte:N=` for
whole numbers, and `default=` for everything else. A label may use
`{{ value }}`, as in `default={{ value // 1000000 }} Mbps`. Colours are green,
red, yellow, orange and blue. A value no rule matches shows as sent.

Value mappings change only what people see — stored data and alert rules read
the raw value, so an alert rule on status still compares against `2`, not
`DOWN`. A dry run shows raw values for the same reason; the mapped label is
checked on the host page after applying.

Applying a template **copies** each value mapping's rules onto the host's items.
Editing a value mapping later changes future applies, not hosts it was already
applied to; re-apply the template to pick the change up. `value-mapping update
--rule` replaces the whole rule list, in the order given.

### 5. Group items into modules, modules into templates

```
ups monitoring module create --name "Cisco interfaces"
ups monitoring template create --name "Cisco IOS switch" --module <module-id>
ups monitoring item create --template <template-id> --module <module-id> --name "Interface counters"
ups monitoring template update <template-id> --publish
ups monitoring template apply <template-id> --host <one-host-id>
```

A module is the group; a template holds modules; an item reaches a template
because its module is in that template's set. Group as the people who maintain
this do: one template per device type, and one module per data source crossed
with a natural grouping - SNMP interfaces, SNMP environmentals (CPU, memory,
temperature, power), ICMP availability. Ask before inventing a different split;
existing names on the organization show the convention in use. Deleting a module deletes every
item in it, including the copies applied to hosts (`module delete` names them). A module added to two templates
carries its items into both — the point when the checks really are the same,
and a surprise when they are not.

### 6. Handing an item to the portal

A person finishing an item in the web UI sees its response in the mapping step,
and that list comes from the item's last **test** result. A dry run writes no
such result, so an item built here opens with nothing to map until you run:

```
ups monitoring item test <item-id>
```

Two more fields the portal's wizard expects, both of which `ups` sets: the
credential tag (`--credential-tag`) and the frequency (`--interval`). Without
them the wizard cannot move past its first step.

### 7. Put the device in monitoring

```
ups host update <host-id> --monitoring
```

This is the switch the agent obeys. A host that is not in monitoring is left
out of the payload the agent polls, so every item on it fetches nothing and
nothing says so - the same silent gap as a missing schema mapping. Check it
with `ups host show <id>` ("In monitoring"), and finish here rather than
assuming an applied template is enough.

`--monitoring=false` takes it back out and stops every check on the host, so
it confirms first.

Apply to **one** host and dry-run there before rolling out. Then confirm the data
arrived, with the labels people will see: `ups monitoring schema data <host-id>
<schema-id>` shows the newest row per identifier, value mappings applied. No rows
after two polling intervals means nothing reached the portal. A template item has
no device to poll, so nothing has checked it until it lands on one. Applying it
to fifty hosts first produces fifty unverified checks, and per the coverage
rule, no alert about any of them.

## Diff before apply, always

Infrastructure-as-code is the primary way to make bulk changes:

```
ups export --out ./infra/     # pull current state to YAML
ups diff ./infra/             # read this. every time.
ups apply ./infra/ --yes      # converges the platform to the YAML
```

`--out` takes a directory or a single file. Prefer a directory: one file per host keeps
git diffs small and reviewable.

```
infra/
  infrastructure.yaml     # apiVersion and which infrastructure this is
  templates/
    cisco-ios-switch.yaml # one monitoring template, with its checks nested
  hosts/
    core-sw-01.yaml       # one host, with its monitoring items nested
    fw-01.yaml
```

`diff` and `apply` accept either form. Re-exporting into a directory deletes host files
whose resource is gone from the platform, and says which — a leftover file would read as a
host to create on the next apply.

`apply` is idempotent and safe to re-run. It is **not** safe to run unread. The diff is
the entire safety mechanism, because the export covers a whole infrastructure — a deleted
YAML block means a deleted resource, and per the coverage rule above, deleted monitoring
is a silent failure.

`apply` refuses destructive diffs unless given `--allow-delete`. Do not add that flag to
get past an error. If the diff proposes deletions the user did not intend, the YAML is
wrong — fix the YAML.

If apply fails partway it stops at that step and names it. Nothing is rolled back, so
re-run `ups diff` to see what remains rather than assuming either outcome.

### Monitoring templates in the document

A host records the template applied to it in a `template:` field, and the export writes out
the templates its hosts actually use — not the whole library. Editing a check in
`templates/` is then one small diff, instead of the same edit repeated on every host that
carries it.

Two asymmetries here are deliberate, and both matter:

- **A `template:` change on a host is destructive.** It reads as a one-field update, but the
  platform carries it out by deleting every monitoring item on that host and copying the
  template's in. `ups diff` lists the items that will be lost under the step, counts them,
  and `apply` refuses without `--allow-delete`. Read that list. While a host's template is
  changing, its inline `monitoring:` items are not diffed at all — the template is the
  source of truth for them from that point on.
- **A template missing from the document is never deleted.** Templates belong to the
  organization, not to one infrastructure, so a file that stops mentioning one means "not
  managed here", never "remove it". Deleting a template is an explicit act:
  `ups monitoring template delete`. The converse is that editing a template *does* change it
  everywhere it is used, including hosts this document does not list — `ups diff` says so
  whenever a plan touches one.

Checks removed from a template the document *does* declare are deleted, and count as
destructive like any other removal.

### Renaming

Exported documents carry an `id:` on each host and monitoring item. **Keep it.** With the
id present, changing `name:` is a real rename: one update, the resource and its monitoring
history survive.

Strip the ids and identity falls back to name, where a rename is indistinguishable from
"delete this, create that" — the diff will show a delete plus a create, and `ups diff`
warns when a pair looks like an accidental rename. If you see that warning, do not proceed:
restore the `id:` field instead.

Drop the ids only when you deliberately want a portable template to apply to a *different*
infrastructure.

## Logs: two backends, and you must read which one answered

There are two log APIs, and which one answers depends on the server:

- **`POST /api/logs/search/`** searches Elasticsearch. Filters, time bounds and ordering
  are applied server-side, across the whole index.
- **`GET /api/logs/`** is the original endpoint. It accepts **no query parameters**, so
  the CLI fetches records and filters them locally.

`ups logs search` tries the search endpoint and falls back when the server does not have
it. The fallback is reported on stderr, never silent. **Read that line before you trust
the result**, because the two backends answer different questions:

| | Search endpoint | Fallback |
|---|---|---|
| Scope | the whole index | only the records one fetch returned |
| `--host` | exact host match | substring match |
| `--query`, `--dataset`, `--sort` | applied | **ignored, and named on stderr** |
| An empty result means | nothing in the index matched | nothing *fetched* matched |

That last row is the one that matters. On the fallback, a client-side filter cannot match
a record it never fetched, so an empty result is not evidence that nothing else matched.
Say so when you report it rather than presenting it as conclusive.

Only a missing endpoint (404/405/501) causes a fallback. A rejected query, an auth failure
or a broken index fails the command — a failing search is never downgraded into a quiet
one.

Flags:

- `--query` takes an Elasticsearch query string; `--text` is the plain-substring form and
  works on both backends.
- `--dataset` is `flow`, `monitoring` or `syslog`, repeatable. `--host` and `--level` are
  repeatable too.
- `--since` and `--until` take either a duration (`1h`) or a timestamp (`2026-09-04T08:00:00Z`).
- `--search-mode server` refuses to fall back — use it when a weaker answer would be worse
  than no answer. `--search-mode client` forces the old path.
- Always bound the query with `--limit`. `ups` caps traversal and reports when results were
  truncated. Truncated is not "no matches" — pass that distinction on to the user.

The search endpoint scopes by numeric infrastructure id, so an infrastructure must be
selected; `ups` refuses to send an unscoped search rather than widening it to everything
you can read.

There is no log-based device discovery. Discovery is topology scanning — see `ups discovery`.

## Names that look alike but are not

| Looks similar | Actually |
|---|---|
| `ups doctor` | Checks **your local setup** — config, auth, context, this skill, and whether a newer CLI is out. Touches nothing remote except to verify the token and read the release feed. |
| `ups infra healthcheck` | Starts a **platform-side scan of an infrastructure**. A real operation against the customer's environment, and it needs API credentials on the infrastructure. |
| `/api/status/` | Ticket statuses (a lookup table), not system health. There is no `ups` command for it. |
| monitoring **module** | The definition of *what* to check. |
| monitoring **item** | An instance of a module bound to a host + credential. |
| monitoring **event** | A fired alert. |
| data **schema** | The named fields a check publishes into. Shared: graphs and alert rules read them by name. |
| schema **mapping** | One item's wiring from response paths onto those fields. Per item, not shared. |
| **value** mapping | How a field's raw value reads on the portal (`1` → UP in green). Shared by name; copied onto hosts when a template is applied. Changes nothing stored or alerted on. |
| `ups mib walk` | Reads the local MIB cache. Offline, and never touches the device. |
| `ups host walk` | Asks the device, through the agent, what it returns. Nothing is saved or published. |
| `data_source` / `action_type` / worker name | An item's data source. `data_source` (API, SNMP, ICMP) is the current field and what the dry run reads; `action_type` is the legacy one the server derives from it; a dry-run trace names the worker (`snmpstd`, `api_data`, `icmp`). |
| `item dry-run` | Runs the whole pipeline, publishes nothing, tells you whether the config collects data. |
| `item test` | Fetches the raw response and stops. Cannot tell you whether the config collects data. |
| `change` | The planned or recorded work. |
| `change_log` | The field-level audit trail of what was actually mutated. |
| `ups event silence` | Mutes one event. For planned work use a maintenance window instead — it covers every host you are touching. |

Confusing `doctor` with `infra healthcheck` means running a live scan when the user asked
you to check their config. Do not.

## Which server am I talking to?

The API URL is configurable, and there is usually more than one: production, staging, and
self-hosted or on-prem installations. It resolves in this order, highest wins:

```
--api-url <url>            flag
UPSTACKED_API_URL          environment
profile in config file     ups profile use <name>
```

`ups context show` prints the active URL **and where it came from**. Check it before any
write. "Staging" and "production" differ by one line of config and nothing in the prompt.

Credentials are stored per profile and bound to the URL that issued them. `ups` will not
send a token to a host it was not issued for — if you switch URL and get an auth failure,
that is the safeguard working, not a bug. Run `ups login` against the new host.

When a user says "check X" and the active profile is production, and the task looks
exploratory or experimental, confirm the target before proceeding.

## Output and scripting

- Default output is human tables. Pass `--json` for anything you intend to parse. Never
  parse the table output — it is not a stable interface.
- `--id-only` emits bare IDs for piping. On a `create` it prints the new record's id, and
  `--json` prints the record: `mod=$(ups monitoring module create --name X --id-only)`.
- Exit codes are meaningful: `0` ok, `1` general failure, `2` usage error, `3` auth
  failure, `4` not found, `5` conflict/precondition failed. Check them; do not grep stderr.
- Progress, warnings and truncation notes go to stderr; data goes to stdout. Piping stdout
  is safe.
- Every list command paginates and is capped by `--limit` (default 100). Truncation is
  always reported. Report it onward.
- `--dry-run` shows what a mutating command would send without sending it. Prefer it when
  the user's intent is ambiguous.
- `ups event watch` and `ups logs follow` render live tables and cannot emit `--json`.
  For scripted polling call the plain `list`/`search` form on an interval instead.

When a command is denied, `ups whoami` shows the roles actually granted, which is usually
the answer to "why can't I do this".

## When something is not behaving

`ups doctor` answers "is the setup correct". `ups debug` answers "what actually
happened", which is where a support conversation starts.

```
ups debug info                   # version, server and where it came from, context, caches
ups debug log                    # recent invocations: command, exit code, request count
ups debug log --failed --json    # the failures, with method, path and status
ups debug bundle                 # both, in one block to paste into a report
```

Every invocation is recorded locally to the config directory: the command, its exit code,
and the method, path and status of each API request. Response bodies are never recorded —
they carry customer data, and a debug log is the last place it should be duplicated.
Nothing is sent anywhere; `UPS_NO_HISTORY=1` turns recording off.

Read `ups debug log --failed --json` before guessing at a failure. A 403 and a 400 need
different answers — one is a permission the account does not have, the other is a request
the server would not accept from anyone — and the status code separates them faster than
re-reading the command. When handing a problem to someone else, send `ups debug bundle`
rather than a description of it. Read it first: it holds no tokens and no response bodies,
but command arguments name hosts and customers.

## When to stop and ask

Stop and ask the user rather than guessing:

- A name resolves to multiple hosts, or to a host in an unexpected customer.
- A diff proposes deleting monitoring items, hosts, or credentials that the user did not
  explicitly ask to remove.
- A runbook preflight reports missing credentials.
- A monitoring item was created but its dry run collected nothing, or the dry run was
  refused and only the weaker test ran.
- A mapping's paths resolve to nothing in the dry run, or a multi-valued mapping has
  no identifier and the user has not said the response is single-row.
- `ups mib show` cannot resolve the OID you were about to poll.
- A legacy `--data-source` type matched more than one action. Ask which; they
  collect different things.
- The active context is not the infrastructure the user seems to be talking about.
- The active API URL is production and the request looks exploratory or experimental.
- An operation would affect more than a handful of hosts and the user did not name a bulk
  operation.
- You are about to pass `--allow-delete`, `--skip-preflight` or `--skip-test` to get past
  an error rather than because the user asked for it.

The cost of asking is one message. The cost of a wrong write is a customer-facing incident.

## Getting set up

```
ups init --api-url <url>   # server, auth, context, and install this skill
ups doctor                 # verify all of it; non-zero exit if anything is wrong
ups context show           # which server and infrastructure am I pointed at?
ups mib sync               # cache the MIBs, if you will be authoring SNMP checks
```

This skill installs into whichever LLM clients the user works with — Claude Code gets a
real skill, and other tools get the same guidance in their own convention:

```
ups skill install                       # pick clients interactively
ups skill install --client claude,agents
ups skill clients                       # what can be installed where
ups skill status                        # where it is, and whether it is current
```

Files shared with the user's own instructions (AGENTS.md, GEMINI.md, Copilot instructions)
are edited in place: only a marked block is managed, and everything around it is left
alone. Never rewrite one of those files wholesale.

If `ups doctor` reports this skill is outdated, run
`ups skill install --client <id> --force`. A skill that describes a different command
surface than the installed binary is worse than none.

`doctor` also says when a newer CLI has been released
(`brew upgrade --cask upstacked/tools/cli`). It is a warning, never a failure, and is
skipped when the release feed cannot be reached, so `doctor` still works offline.
