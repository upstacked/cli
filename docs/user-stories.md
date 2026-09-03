# Upstacked CLI — User Stories

Status: **draft for review**. Nothing here is built yet.

The API surface is `docs/Upstacked.yaml` (OpenAPI 3.0.3, ~200 endpoints, ~30 resource
families). Every story below is anchored to endpoints that exist in that spec, except
where a section says otherwise (see L).

## Personas

| ID | Persona | Shape of use |
|----|---------|--------------|
| P1 | **On-call / NOC engineer** | Interactive, TTY-first, incident-driven. Short commands, fast answers. |
| P2 | **Consultant onboarding a customer** | Bursty create-heavy work: discovery, devices, credentials, monitoring, handover docs. |
| P3 | **Platform engineer in CI** | Non-interactive, scripted, idempotent. Token from env, JSON out, exit codes that mean something. |

All three are in scope. The proposed sequencing is in [Delivery order](#delivery-order).

## Cross-cutting requirements

These are not polish. They are the contract every command below is written against, and
they come from <https://clig.dev>.

| ID | Requirement |
|----|-------------|
| X1 | Human-readable tables on a TTY; `--json` everywhere; auto-detect non-TTY and drop colour/spinners. |
| X2 | `--id-only` on every list command, so `ups host list --id-only \| xargs` composes. |
| X3 | Config precedence: flags > env (`UPSTACKED_*`) > config file > defaults. `--help` states which won. |
| X3a | API base URL is configurable at every level (`--api-url`, `UPSTACKED_API_URL`, per-profile config) with a sane default. Supports prod, staging, and self-hosted installs. |
| X3b | Tokens are stored per profile and bound to the URL that issued them. The CLI never sends a token to a host it was not issued for. |
| X4 | Destructive operations confirm interactively; `--yes` skips; `--dry-run` previews. Never prompt when stdin is not a TTY — fail with a clear message instead. |
| X5 | Errors say what failed, why, and what to try next. HTTP status codes never leak raw. |
| X6 | Distinct exit codes: 0 ok, 1 general, 2 usage, 3 auth, 4 not-found, 5 conflict/precondition. |
| X7 | Secrets never appear in argv, shell history, or terminal echo. Read from stdin or env only. |
| X8 | Shell completion for bash/zsh/fish, including dynamic completion of infra and host names. |
| X9 | Every command that hits the network respects `--timeout` and can be interrupted cleanly. |
| X10 | Every `ups` command string appearing in the shipped agent skill must correspond to a real command. Enforced by an integration test, not by review. |

## A. Foundations

| ID | Story | Endpoints |
|----|-------|-----------|
| A1 | As any user, I log in once — including MFA — and the CLI refreshes my token silently so I am not re-prompted mid-task. | `POST /api/token/`, `POST /api/token/refresh/`, `POST /api/auth/code/request/`, `POST /api/auth/login/code/` |
| A2 | As P3, I authenticate headlessly from `UPSTACKED_TOKEN` with no interactive step and no keychain access. | same as A1 |
| A3 | As any user, I set a default infrastructure (and customer) once, so I do not repeat `--infra` on every command. I can override per-command and see which context is active. | `GET /api/infrastructure/`, local config |
| A4 | As any user, I run `ups whoami` and see who I am, which org, which roles, and which feature flags — so "why can't I do this" is answerable without support. | `GET /api/user/details/v2/`, `GET /api/permissions/roles/`, `GET /api/feature-flags` |
| A5 | As any user, I switch between multiple profiles (prod/staging, or several customers) without re-authenticating each time. | local config |
| A6 | As any user, I point the CLI at a different Upstacked server — staging, or a self-hosted install — without editing files by hand. | X3a |
| A7 | As any user, `ups context show` tells me which server and infrastructure are active **and where each value came from**, so I can tell prod from staging before I write. | X3a, A3 |

**Note on A3.** Nearly every endpoint is scoped by `infrastructure_id`, and many also by
`customer`. Whether context is persistent or per-invocation is the single biggest
ergonomic decision in the tool. Persistent context with a visible indicator and an
explicit override is the recommendation.

## B. Triage (P1)

| ID | Story | Endpoints |
|----|-------|-----------|
| B1 | As P1, I see what is broken right now across an infrastructure, sorted by severity. | `GET /api/monitoring/current_incidents/`, `GET /api/monitoring_event/open_events/`, `GET /api/monitoring/host_status/` |
| B2 | As P1, I drill into one host: current state, core metrics, recent incident history. | `GET /api/monitoring-metrics/host/{id}/core-metrics/`, `/incident-history/`, `GET /api/host/{id}/` |
| B3 | As P1, I mark an event recovered, or silence its alerts, without opening the web UI. | `POST /api/monitoring_event/{id}/recover/`, `/{id}/toggle_alerts/` |
| B4 | As P1, before I touch anything, I answer "what changed here recently?" | `GET /api/change_log/` (filters: `changed_field`, `new_value`, `change_by`, `created_gte`) |
| B5 | As P1, I follow events live in my terminal (`--follow`) while working an incident. | polling `open_events` |
| B6 | As P1 or P2, I schedule a maintenance window over a set of hosts so my own work does not page anyone. | `POST /api/maintenance/` (takes `hostids`) |

B6 is the natural partner to the change-management stories in section F.

## C. Discovery and onboarding (P2)

| ID | Story | Endpoints |
|----|-------|-----------|
| C1 | As P2, I start a network discovery against a customer network and watch it progress. | `POST /api/discovery/`, `GET /api/discovery/start/`, `POST /api/onboarding/network-discovery/` |
| C2 | As P2, I review what a discovery found and promote selected results into real hosts. | `GET /api/discovery/{id}/`, `POST /api/onboarding/resource-create/`, `POST /api/host/` |
| C3 | As P2, I inspect the discovered topology and how devices link together. | `GET /api/topology/get/`, `GET /api/host_link/`, `GET /api/monitoring/flow/topology/` |
| C4 | As P2 or P1, I trace the path to a host to understand its position in the network. | `GET /api/infrastructure/{id}/hosts/{host_id}/trace/` |

**Correction to the original brief.** "Search the logs to discover devices" does not map
to this API. Discovery here is topology/network scanning, not log mining. If log-derived
discovery is a real requirement it needs a separate mechanism — see the BLOCKED section.

## D. Inventory: devices, APIs, credentials (P2)

| ID | Story | Endpoints |
|----|-------|-----------|
| D1 | As P2, I add a device (host) with its vendor, model, OS, and addresses. | `POST /api/host/`, `GET /api/host_vendor/`, `/api/host_model/`, `/api/host_os/`, `/api/host_type/` |
| D2 | As P2, I attach a monitoring item to a host — including a REST/HTTP check — and pick its module and interval. | `POST /api/monitoring/items/`, `GET /api/monitoring/modules/`, `/api/monitoring/intervals/` |
| D3 | As P2, I test a monitoring item against a live host **before** saving it, and see the raw response. | `GET /api/monitoring/item/{id}/test`, `POST /api/orchestration/snmp_runbook_elements/{id}/test/` |
| D4 | As P2, I store credentials (SNMP v2/v3, device, API, OAuth2, vendor-specific) without ever putting a secret on the command line. | `POST /api/credential/{type}/` (14 types), `POST /api/credential/credentials/bulk-create/` |
| D5 | As P2, I author a monitoring template and apply it to hosts instead of building items one at a time. | `/api/monitoring/templates/`, `/api/monitoring/templates/{id}/`, `/api/monitoring/templates/infra-system-credential/`, `POST /api/monitoring/items/` (host-less), `PATCH /api/host/{id}/` |
| D6 | As P2, I update many hosts at once, previewing the change first. | `POST /api/host/bulkupdate/`, `POST /api/asset/bulk-update/` |
| D7 | As P2, I link assets to hosts and keep them in sync. | `POST /api/asset/{id}/link-unlink/`, `/api/asset/{id}/sync-host/` |

D4 carries X7 as a hard constraint: secrets come from stdin or env, never argv.

## E. IPAM and bulk import (P3)

| ID | Story | Endpoints |
|----|-------|-----------|
| E1 | As P3, I claim the next free address in a subnet and assign it, in one scriptable command. | `GET /api/ipam/subnet/{id}/next/`, `POST /api/ipam/ip_address/` |
| E2 | As P3, I browse and manage subnets and subnet groups, and apply subnet templates. | `/api/ipam/subnet/`, `/api/ipam/subnet_group/`, `/api/ipam/subnet_group/template/{id}/apply/` |
| E3 | As P3, I bulk-import assets from CSV with a **validate-then-apply** flow, so I see errors before anything is written. | `POST /api/assets/import/validate/`, `POST /api/assets/import/`, `GET /api/assets/import/download-sample/`, `GET /api/assets/import/{id}/rows/` |
| E4 | As P3, I back up and restore a customer's IPAM data. | `GET /api/ipam/customer/{id}/backup/`, `POST /api/ipam/customer/{id}/restore/` |

E1 is the most naturally CLI-shaped operation in the entire API: one input, one output,
composable, no UI equivalent that is faster.

E3 already has the plan/apply shape server-side. The CLI should expose it as exactly that.

## F. Change management (P1, P2)

| ID | Story | Endpoints |
|----|-------|-----------|
| F1 | As P1, I open a change, move it through its lifecycle, and close it — from the terminal. | `GET/POST /api/change/`, `GET/PUT /api/change/{id}/` |
| F2 | As P1, I list changes filtered by customer, infra, performer, and date window. | `GET /api/change/` (rich filter set) |
| F3 | As P1, I see the audit trail of what changed, by whom, field by field. | `GET /api/change_log/` |
| F4 | As P1, a change automatically carries a maintenance window so alerts are suppressed for its duration. | `POST /api/maintenance/` + F1 |

## G. Runbooks and automation (P1, P3)

| ID | Story | Endpoints |
|----|-------|-----------|
| G1 | As P1, I run a runbook against an infrastructure and follow its results live. | `POST /api/orchestration/runbook/{id}/start/{infra_id}/`, `/results/last/`, `/results/{id}/` |
| G2 | As P1, I cancel a running runbook or pipeline. | `POST /api/orchestration/runbook/{id}/results/{id}/cancel/`, `/runbook_pipeline_runs/{id}/cancel/` |
| G3 | As P1, I check a runbook's **missing credentials before running it**, so it does not fail halfway. | `GET /api/orchestration/runbooks/{id}/missing-credentials/` |
| G4 | As P3, I run a pipeline and resume it after a failure. | `/api/orchestration/runbook_pipelines/`, `/runbook_pipeline_runs/{id}/resume/` |
| G5 | As P3, I manage automation policies and their rules, including enable/disable. | `/api/automation/policies/`, `/api/automation/policy/{id}/active-inactive-status/`, `/policy/{id}/rules/` |
| G6 | As P3, I debug a webhook by reading its delivery log. | `GET /api/webhook/{id}/logs/`, `/api/webhooks/` |

G3 is the "preflight" pattern — cheap to build, high trust payoff.

## H. Infrastructure as code (P2, P3) — the documentation story

The chosen interpretation of "document their infrastructure" is **export/import as code**:
infrastructure lives as YAML in git, and the CLI reconciles it against the platform.

| ID | Story | Endpoints |
|----|-------|-----------|
| H1 | As P2, I export an existing infrastructure to YAML files I can commit to git. | reads across `/api/host/`, `/api/monitoring/items/`, `/api/host_link/`, `/api/core/extended_attributes/` |
| H2 | As P3, I diff my local YAML against the live platform and see exactly what would change. | same reads, local diff |
| H3 | As P3, I apply my local YAML and have the platform converge to it — idempotently, re-runnable, safe to run in CI. | writes across the same resources |
| H4 | As P3, apply refuses to proceed on a destructive diff unless I opt in explicitly. | X4 |
| H5 | As P2, I round-trip: export, edit, apply, re-export, and the result is stable (no spurious diff). | — |

**This is the largest piece of work in the backlog.** It needs a canonical resource model,
stable local-to-remote identity mapping, and a deterministic diff. The API returns integer
IDs, so identity mapping is the hard problem: local YAML must key on something durable
(name within infrastructure, or an annotation the platform stores) rather than IDs assigned
by the server.

H5 is the acceptance test that matters. If round-trip is not stable, nothing else in
section H is trustworthy.

Secondary documentation surfaces, if wanted alongside:
`/api/knowledgebase/`, `/api/sd/knowledgebase_entry/`, `/api/core/extended_attributes/`,
`/api/host/additional_fields/`.

## I. Tickets, service desk, reporting

| ID | Story | Endpoints |
|----|-------|-----------|
| I1 | As P1, I list, filter, comment on, and resolve tickets. | `/api/ticket/`, `/api/ticket/resolutions/`, `/api/comment/` |
| I2 | As P1, I log work activity (time) against a ticket or milestone. | `/api/ticket/workactivity/`, `/api/project/milestone/{id}/work_activity/` |
| I3 | As P1, I merge duplicate tickets or split an event out of one. | `/api/ticket/merge/existing/`, `/merge/new/`, `/eventsplit/existing/`, `/eventsplit/new/` |
| I4 | As P3, I generate an availability or change report for a period and emit JSON/CSV from a scheduled job. | `/api/reporting/availability/`, `/api/reporting/changes/`, `/api/reporting/templates/` |
| I5 | As P1, I check which of my devices are exposed to a given CVE. | `GET /api/cve/` |

## J. Agent skill, init, and doctor

The CLI ships an LLM skill describing how to operate Upstacked safely, installs it, and
verifies it. The skill's job is to convey **why** the workflows are shaped as they are —
an agent can read `--help` for the *how*, but cannot derive from any API response that
deleted monitoring fails silently, or that a half-run runbook leaves a device partially
configured.

Draft: `internal/skill/SKILL.md`.

| ID | Story | Notes |
|----|-------|-------|
| J1 | As an LLM agent operating `ups`, I have a skill that explains the resource model and the reasoning behind each guardrail, so I do not have to infer intent from endpoint names. | Skill content |
| J2 | As any user, `ups init` gets me from nothing to working: server URL, login, default context, and the skill installed. Re-running it is safe. | Idempotent |
| J3 | As any user, I install or update the skill on its own, at user or project scope. | `ups skill install [--scope user\|project] [--force]` |
| J4 | As any user, `ups doctor` tells me everything that is wrong with my setup at once, with a concrete fix for each — not the first error and then a stop. | Non-zero exit on any failure |
| J5 | As P3, `ups doctor --json` runs in CI and fails the build on a broken or stale setup. | X1, X6 |
| J6 | As any user, `doctor` distinguishes an **outdated** skill from one I have **deliberately edited**, and does not silently overwrite my edits. | Checksum + version manifest |
| J7 | As a maintainer, the skill cannot drift from the real command surface, because CI fails when it does. | X10 |

### `ups init`

Orchestration only — it delegates to commands that also stand alone (`login`,
`context set`, `skill install`). Keeping them separable is the Simple Made Easy call: one
convenient entry point, no hidden behaviour that cannot be invoked directly.

```
ups init [--api-url URL] [--skill-only|--no-skill] [--scope user|project] [--non-interactive]
```

Non-interactive mode must never prompt (X4) — it fails with a message naming the missing
flag instead.

### `ups doctor`

Checks, each reported independently with pass / warn / fail and a fix hint:

| # | Check | Fail means |
|---|-------|-----------|
| 1 | Config file exists and parses | Cannot proceed |
| 2 | API URL resolves and is reachable | Wrong URL, or network |
| 3 | Token present, valid, and bound to the active URL | Re-login needed |
| 4 | Token not expiring imminently | Warn only |
| 5 | Active context resolves to a real infrastructure/customer | Stale context |
| 6 | Roles/permissions sufficient for common operations | Explains "why can't I" up front |
| 7 | Skill installed, and in a location the agent will find | Agent operating blind |
| 8 | Skill version matches the binary | Stale guidance — actively dangerous |
| 9 | Skill checksum: unmodified / locally edited | Warn, never overwrite silently |
| 10 | Shell completion installed | Warn only |

Reachability probe is `GET /api/user/details/v2/`. **Not** `/api/healthcheck/start/` — that
starts a platform-side scan of a customer infrastructure (see J-note below). `/api/status/`
is ticket statuses, not system status.

`doctor` never mutates. A separate `ups doctor --fix` may apply the safe subset, and must
confirm before touching a locally-edited skill.

### Naming collision to avoid

`ups doctor` (local setup) and the platform's infrastructure healthcheck
(`GET /api/healthcheck/start/`, state on the `Infrastructure` schema: `healthcheck_state`,
`healthcheck_running`, `healthcheck_progress_percentage`, `healthcheck_log`) are unrelated.
The platform one is a live operation against a customer environment and requires API
credentials on the infrastructure. Proposed name: `ups infra healthcheck`. The skill calls
this out explicitly, because an agent will otherwise conflate them.

| ID | Story | Endpoints |
|----|-------|-----------|
| J8 | As P2, I start a platform healthcheck against an infrastructure and watch its progress and log. | `GET /api/healthcheck/start/`, `Infrastructure.healthcheck_*` |

## L. Log search — degraded until the API lands

The real log API does not exist yet. Decision: **build against `/api/logs/` now**, and
design the command surface so it survives the real API arriving.

`GET /api/logs/` takes no query parameters and declares no response schema. So:

| ID | Story | Constraint |
|----|-------|-----------|
| L1 | As P1, I search logs for an infrastructure by time range, host, and text. | Filtering is **client-side**. The CLI fetches, then filters locally. |
| L2 | As P1, I follow logs live while working an incident. | Polling, not streaming. |

Design consequences, all of which need to hold now so the real API is a drop-in later:

- Filtering lives behind an interface with one implementation (client-side) today and a
  server-side implementation later. The command surface must not change when it swaps.
- Traversal is capped (Tigerstyle: explicit limits). Truncation is **reported**, never
  silent — a truncated result set is not "no matches", and both the user and any agent
  must be told the difference.
- No filter is promised that is not actually applied. Client-side filtering over a
  partial fetch can miss matches it never saw.
- The response shape is undocumented, so the client tolerates unknown fields rather than
  failing to parse.

**Dropped:** log-based device discovery. It has no endpoint, and discovery in this API is
topology scanning (section C).

## Delivery order

Proposed. Each phase ships something usable on its own.

**Phase 0 — Foundations.** A1–A7, J1–J7, plus the X-series contract. Nothing works without
auth, server URL, context, and the output/error model. The skill and `doctor` belong here,
not at the end: the skill is written against the command surface, so drafting it early
forces that surface to be coherent, and X10 keeps it honest from the first commit.

**Phase 1 — Triage (P1).** B1–B6, F1–F3, J8. Read-mostly, so it exercises the whole client
stack cheaply and delivers daily value from week one. Fastest path to real feedback.

**Phase 2 — Inventory and onboarding (P2).** C1–C4, D1–D7, E1–E4, L1–L2. Introduces writes,
bulk operations, and confirmation flows. Builds the resource model that Phase 3 needs.

**Phase 3 — Infrastructure as code (P2/P3).** H1–H5, G1–G4. The payoff, and it is only
safe to attempt once Phase 2 has settled the read/write model for every resource type.

**Phase 4 — Depth.** I1–I5, G5–G6, and the server-side swap for L1–L2 when the real log
API ships.

Rationale for starting with P1 despite "all three personas": triage is the cheapest phase
that proves the entire stack end to end, and P2/P3 work depends on a resource model that
Phase 2 has to establish anyway.

## Design principles in force

**Simple Made Easy.** Keep these separate and independently testable: the HTTP transport,
the resource model, the diff engine, the output renderer, and the command layer. Do not let
CLI flag structs leak into the API client, and do not let API response shapes leak into
rendering.

**Tigerstyle.** Explicit limits everywhere (page sizes, retry counts, timeouts, max diff
size). Assert preconditions at function boundaries. No unbounded loops over paginated
endpoints without a declared cap. Deterministic behaviour — same input, same output,
including ordering of exported YAML.

**clig.dev.** See the X-series above.

**Testing.** Integration-first: run the real command surface against a recorded or stubbed
API and assert on stdout, stderr, and exit code. Golden files for human output, schema
assertions for `--json`. Unit tests only where logic is genuinely non-trivial (the diff
engine, identity mapping, pagination).
