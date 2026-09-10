---
name: upstacked
description: Operate Upstacked infrastructure via the `ups` CLI — devices, monitoring, credentials, IPAM, changes, runbooks, tickets, and infrastructure-as-code. Use whenever the task involves the `ups` command, Upstacked hosts/infrastructures/monitoring, or a user asks to inspect, change, or document network infrastructure managed by Upstacked.
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

A host-less template item cannot be checked — there is no device to poll until it is
applied — so the usual create-then-dry-run feedback loop does not run. That is exactly why
a template should be applied to one host and dry-run there before it is rolled out to the
rest.

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
| `ups doctor` | Checks **your local setup** — config, auth, context, this skill. Touches nothing remote except to verify the token. |
| `ups infra healthcheck` | Starts a **platform-side scan of an infrastructure**. A real operation against the customer's environment, and it needs API credentials on the infrastructure. |
| `/api/status/` | Ticket statuses (a lookup table), not system health. There is no `ups` command for it. |
| monitoring **module** | The definition of *what* to check. |
| monitoring **item** | An instance of a module bound to a host + credential. |
| monitoring **event** | A fired alert. |
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
- `--id-only` emits bare IDs for piping.
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

## When to stop and ask

Stop and ask the user rather than guessing:

- A name resolves to multiple hosts, or to a host in an unexpected customer.
- A diff proposes deleting monitoring items, hosts, or credentials that the user did not
  explicitly ask to remove.
- A runbook preflight reports missing credentials.
- A monitoring item was created but its dry run collected nothing, or the dry run was
  refused and only the weaker test ran.
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
