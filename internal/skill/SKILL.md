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
and you get either silence or false alerts. Testing is the only feedback loop that exists:
afterwards there is no indicator distinguishing "healthy" from "never collected anything".

`ups monitoring item create` therefore tests the item it just made, and prints the
response. Read it. If the test fails, the item exists but is collecting nothing — fix it
or remove it, and tell the user; do not leave it and move on.

```
ups monitoring item create --host <id> --name "CPU" --module <id>   # creates, then tests
ups monitoring item test <item-id>                                  # re-test an existing item
```

The API cannot test a configuration that has not been saved, so there is no way to check
one before creating it. `--skip-test` exists, but using it means nobody has confirmed the
check works.

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

## Logs: filtering is client-side right now

The log API is not built yet. `ups logs` reads `/api/logs/`, which accepts **no query
parameters** — the CLI fetches and filters locally.

Consequences you must account for:

- Filtering is not free. Never call `ups logs` in a loop over many hosts; fetch once and
  narrow with `--text` or `--host`.
- Always bound the query with `--limit` and `--since`. `ups` caps traversal and reports
  when results were truncated. Truncated is not "no matches" — pass that distinction on to
  the user rather than reporting an empty result as conclusive.
- Do not promise a filter that is not applied server-side; the set may be incomplete in
  ways client-side filtering cannot detect.

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
- A monitoring item was created but its test returned nothing.
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

If `ups doctor` reports this skill is outdated, run `ups skill install --force`. A skill
that describes a different command surface than the installed binary is worse than none.
