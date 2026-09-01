---
name: upstacked
description: Operate Upstacked infrastructure via the `ups` CLI — devices, monitoring, credentials, IPAM, changes, runbooks, tickets, and infrastructure-as-code. Use whenever the task involves the `ups` command, Upstacked hosts/infrastructures/monitoring, or a user asks to inspect, change, or document network infrastructure managed by Upstacked.
---

# Upstacked CLI

`ups` manages monitored network infrastructure: real devices, real alerts, real people
who get paged. Most commands here touch production. This document is mostly about **why**
the workflows are shaped the way they are — the `how` is in `ups <command> --help`, which
is always more current than this file.

## Mental model

An **infrastructure** is the top-level scope. It belongs to a **customer**. Almost
everything else hangs off an infrastructure:

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

### Test a monitoring item before saving it

```
ups monitoring item test --host <host> --module <module> --params ...   # inspect the raw response
ups monitoring item create ...                                          # only then
```

A misconfigured item does not error. It returns nothing, or it returns the wrong field,
and you get either silence or false alerts. The test endpoint is the only feedback loop
that exists — there is no "is this working?" indicator afterwards that distinguishes
"healthy" from "never collected anything".

### Preflight a runbook before running it

```
ups runbook preflight <runbook>    # checks for missing credentials
ups runbook run <runbook> --infra <infra>
```

Runbooks execute against live network devices. A run that fails halfway because a
credential was missing can leave a device **partially configured** — worse than not having
run at all. The preflight is cheap. Partial execution is not.

### Validate before importing

```
ups asset import validate <file.csv>   # errors surface here
ups asset import apply <file.csv>
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

### Never put a secret in argv

```
ups credential create snmpv3 --name x --password-stdin < secret   # correct
ups credential create snmpv3 --name x --password hunter2          # WRONG
```

`argv` is visible to `ps`, lands in shell history, and in CI lands in build logs. These
credentials authenticate to live network equipment. Use `--*-stdin` or the documented
environment variables. `ups` will reject secret-bearing flags that receive a literal value.

## Diff before apply, always

Infrastructure-as-code is the primary way to make bulk changes:

```
ups export --infra <infra> --out ./infra/     # pull current state to YAML
$EDITOR ./infra/...
ups diff  ./infra/                            # read this. every time.
ups apply ./infra/                            # converges the platform to the YAML
```

`apply` is idempotent and safe to re-run. It is **not** safe to run unread. The diff is
the entire safety mechanism, because the export covers a whole infrastructure — a deleted
YAML block means a deleted resource, and per the coverage rule above, deleted monitoring
is a silent failure.

`apply` refuses destructive diffs unless given `--allow-delete`. Do not add that flag to
get past an error. If the diff proposes deletions the user did not intend, the YAML is
wrong — fix the YAML.

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

- Filtering is not free. Do not loop `ups logs` in a shell loop over many hosts.
- Always bound the query (`--limit`, `--since`). `ups` enforces a hard cap and will tell
  you when results were truncated. Truncated results are not "no matches" — say so.
- Do not promise the user a filter that is not applied server-side; the result set may be
  incomplete in ways client-side filtering cannot detect.

There is no log-based device discovery. Discovery is topology scanning — see `ups discovery`.

## Names that look alike but are not

| Looks similar | Actually |
|---|---|
| `ups doctor` | Checks **your local setup** — config, auth, context, this skill. Touches nothing remote except to verify the token. |
| `ups infra healthcheck` | Starts a **platform-side scan of an infrastructure**. This is a real operation against the customer's environment. Requires API credentials on the infrastructure. |
| `/api/status/` | Ticket statuses (a lookup table), not system health. There is no `ups` command for it. |
| monitoring **module** | The definition of *what* to check. |
| monitoring **item** | An instance of a module bound to a host + credential. |
| monitoring **event** | A fired alert. |
| `change` | The planned/recorded work. |
| `change_log` | The field-level audit trail of what was actually mutated. |

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
- Every list command paginates. `ups` caps unbounded traversal and reports truncation
  rather than silently returning a partial set. Report truncation to the user.
- `--dry-run` exists on mutating commands. Prefer it when the user's intent is ambiguous.

## When to stop and ask

Stop and ask the user rather than guessing:

- A name resolves to multiple hosts, or to a host in an unexpected customer.
- A diff proposes deleting monitoring items, hosts, or credentials that the user did not
  explicitly ask to remove.
- A runbook preflight reports missing credentials.
- The active context is not the infrastructure the user seems to be talking about.
- The active API URL is production and the request looks exploratory or experimental.
- An operation would affect more than a handful of hosts and the user did not name a bulk
  operation.

The cost of asking is one message. The cost of a wrong write is a customer-facing incident.

## Getting set up

```
ups init --api-url <url>   # server, auth, context, and install this skill
ups doctor                 # verify all of it; non-zero exit if anything is wrong
ups context show           # which server and infrastructure am I pointed at?
```

If `ups doctor` reports this skill is outdated, run `ups skill install --force`. A skill
that describes a different command surface than the installed binary is worse than none.
