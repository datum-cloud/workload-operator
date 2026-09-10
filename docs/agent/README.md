# Agent capabilities

What compute publishes to an AI assistant: the knowledge it reads, the skills it
may follow, and (via `internal/agent`) the diagnosis it can run.

This is the provider side of the Datum AI Agent Framework. A service registers
agent capabilities alongside its catalog registration, entitlement decides which
projects receive them, and the assistant composes a conversation from exactly
the services a project is entitled to. Compute owns what appears here; the
assistant owns the document schema that carries it.

## Contents

| Path | Role |
|---|---|
| `llms-full.txt` | Knowledge. The compute resource model and, critically, how to read its conditions. Fetched over HTTP and appended to the system prompt. |
| `skills/*.md` | Skills. Reviewed, step-by-step triage procedures, loaded on demand. |
| `embed.go` | Embeds both into the binary, so `cmd/compute-mcp` can serve them with no files to mount beside it. |
| `../../internal/agent` | The reason catalog and the diagnosis walk that back the tools. |

## Status

Landed here: the reason catalog, the diagnosis walk, the knowledge and skills
above, and `cmd/compute-mcp` — the MCP server that publishes compute's tools
over Streamable HTTP:

| Tools | Names |
|---|---|
| Diagnosis, read-only | `compute_workloads_list`, `compute_workloads_get`, `compute_instances_list`, `compute_workload_diagnose`, `compute_reason_explain` |
| Discovery, read-only | `compute_locations_list`, `compute_networks_list`, `compute_quota_get`, `compute_instance_types_list` — what a project may place, attach to, afford, and ask for. The locations come from compute's own availability records, so the list is where compute is offered and this project can use it. |
| Planning, writes nothing | `compute_workload_render` (inputs to a manifest, pure), `compute_workload_validate` (the server's verdict on that manifest without creating it) |
| Mutating | `compute_workload_plan`, `compute_workload_apply` |

Every tool is prefixed `compute_`, so the assistant can compose tools from
several services in one conversation without names colliding; the capability
document must register the prefixed names.

## HTTP surface

One process answers everything compute's capability document points at:

| Route | Serves |
|---|---|
| `POST /mcp` | Streamable HTTP MCP, stateless. Requires the caller's bearer token and `X-Datum-Project`. |
| `GET /llms-full.txt` | The knowledge document. Public. |
| `GET /runbooks/<name>.md` | One skill. Public. |
| `GET /healthz` | Liveness. |

The URL says `runbooks` while the directory says `skills`: the path belongs to
the agent framework and is already baked into shipped capability documents, so
it is not compute's to rename. Both document routes are unauthenticated on
purpose — the assistant fetches them to build a system prompt, before it holds
any project context, and they are static text with no tenant data in them.

Three properties of the server are worth knowing before you deploy it:

- **A project is a control plane, not a namespace.** `X-Datum-Project` selects a
  project by rewriting the API host path to
  `/apis/resourcemanager.miloapis.com/v1alpha1/projects/<project>/control-plane`,
  the same rewrite `internal/quota`, `internal/referenceddata` and the datumctl
  plugin perform. Within that control plane compute's objects live in the
  `default` namespace.
- **Every read runs as the caller.** The server holds no credential of its own
  for the project control plane. It takes the bearer token off the request and
  builds a client with it, so a tool call can never see more than the person who
  asked could see themselves, and the server needs no impersonation privilege.
- **The project comes from a header, not a tool argument.** Tool arguments are
  chosen by the model; a model that could name its own project would be one
  prompt injection away from another tenant's workloads. The caller sets
  `X-Datum-Project` after authenticating the user.

Compute publishes exactly two mutating tools, `compute_workload_plan` and
`compute_workload_apply`, and they are deliberately one operation split in half.
`compute_workload_plan` validates a manifest, resolves whether it is a create or an
update, reports whether the network the interface names would have to be
created too, and returns the manifest, the diff, and a plan token — a hash of
that manifest, the project, and the version of the workload it saw.
`compute_workload_apply` accepts that manifest and that token and nothing else, and
re-derives the hash before it writes: a manifest edited after the plan, a token
from another project, or a workload someone else changed in the meantime is
refused. So the only thing apply can produce is the manifest the model already
put in front of the person who asked. A model that reads a poisoned status message cannot
smuggle a different workload past a confirmation of this one, and a manifest
nobody was shown has no token and cannot be applied at all.

The rest of the surface is unchanged by this. Every write runs as the caller,
from the bearer token on the request, so the server holds no credential of its
own and can create nothing the person could not create themselves; the project
still comes from the header. Whether `compute_workload_apply` is offered to a given
project at all is the gateway's decision, from its allow-list — the split above
constrains what a published tool can do, not which projects get it. Adding a
third mutating tool is a new decision and gets its own review: the argument
above is about these two and does not generalise.

## Why the knowledge leads with "how to read conditions"

Compute's top-level condition reasons are deliberately **pointers, not causes**.
`Workload.Available=False` with reason `QuotaNotGranted` names the blocking
subsystem; the real reason — `QuotaExceeded` vs `QuotaNoBudget` vs
`QuotaBackendUnavailable` — lives on an Instance's `QuotaGranted` condition
below it. An assistant that reports the pointer gives a wrong answer that reads
like a right one, so both the knowledge and `internal/agent.Diagnose` are built
around walking through them.

## Skills

Skills use progressive disclosure: only a name and one-line description enter
the system prompt, and the body is fetched when a request matches. That lets
compute publish many procedures at near-zero prompt cost — but only if the
knowledge document does not already carry the procedure. It did, for quota,
referenced data and placement, and a live test showed the model answering a
full runbook question with no skill load at all. `llms-full.txt` is now
orientation and classification; the procedures live here and nowhere else.

| Skill | Covers |
|---|---|
| `workload-not-available` | Top-level triage, symptom to root cause to owner |
| `quota-triage` | `QuotaExceeded` vs `QuotaNoBudget` vs backend faults |
| `instance-not-ready` | `ImageUnavailable`, `InstanceCrashing`, `ConfigurationError` |
| `referenced-data-triage` | Missing, unauthorized, or oversized ConfigMaps/Secrets |
| `placement-triage` | `NoMatchingLocation`, `AmbiguousServingLocation`, `CityCodeMismatch` |
| `stalled-transient` | A transient reason that has outlived its expected window |
| `workload-create` | Deploying something new: prerequisites, the choices that are final at create, and render → validate → show → plan → confirm → apply |

A skill never grants privileges. It can only direct the model toward tools that
are independently on the enforced allow-list, which is why these go through the
same review gate as any published configuration.

## Keeping this honest

Every reason in `internal/agent`'s catalog is classified user-actionable,
platform fault, or transient — the distinction that decides whether a customer
should change their spec or escalate. `TestCatalogCoversEveryAPIReason` parses
`api/v1alpha` and fails when a reason is added without being classified, so the
catalog cannot silently fall behind the API.

A transient reason also declares how long it should take. `stalled` is the
fourth actionability the tools can report, derived at read time when a
condition has held a transient reason past that window — never written in the
catalog and never stored.
`TestTransientReasonsThatSayWaitDeclareAWindow` fails when a reason tells the
reader to wait without saying how long is reasonable, because a transient claim
nobody can falsify is how a wedged workload reads as healthy.

Every reason also has to be *readable*. This text reaches a paying customer
almost verbatim, and the customer deploys workloads — they do not operate Datum.
A term is theirs if they write it in their own workload (`image`, `replicas`,
`configMapRef`, `placement`, `schedulingGates`) or read it in output they
already see (`QuotaExceeded`); it is ours, and banned from the copy, if it only
ever appears inside the implementation (`cell`, `AllowanceBucket`, controllers,
reconcilers, the quota claim). `TestCatalogCopyUsesNoInternalVocabulary`,
`TestDiagnosisCopyUsesNoInternalVocabulary` and
`TestPublishedDocsUseNoInternalVocabulary` fail on the second list, and each
banned term carries the argument for banning it, so the list can be disputed
rather than guessed at. `TestPlainLanguageKeepsTheEvidence` is the counterweight:
object names, reason codes, condition types and durations must survive the plain
English, because those are what a customer escalates with.

When you add a condition reason, add its catalog entry in the same change, give
it a window if it is transient, write the explanation for the customer rather
than for yourself, and update the relevant skill if the triage procedure
changes.
