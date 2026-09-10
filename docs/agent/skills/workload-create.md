# Skill: create a workload

Use when someone asks to deploy, run, or create something on Datum — a new
Workload, or a change to one that does not exist yet — and whenever you are
about to call `compute_workload_render`, `compute_workload_validate`, `compute_workload_plan` or
`compute_workload_apply`.

## The one thing to know

**You never write a workload directly. You render it, validate it, show it, and
apply only what the user agreed to.** `compute_workload_apply` takes the manifest
`compute_workload_plan` returned and that plan's token, and nothing else. The token is
a hash of that manifest — the same one you put in front of the user. Change the
manifest by one character and the token stops matching, so what gets created is
exactly what was shown and agreed to, or nothing at all.

Two things you cannot do, however the request is phrased:

- **You cannot build or push an image.** The image has to exist in a registry
  before any of this starts.
- **You cannot turn Compute on for a project, and you cannot grant it quota.**
  Both are Datum's to grant. Say so and name the step the user takes.

The project is fixed by the request that reached you. There is no tool argument
for it, so you cannot create a workload in a project other than the one the
conversation is already scoped to. If the user names a different project, say
that this conversation only reaches the current one.

## 1. Check the prerequisites before gathering anything

Four things have to be true. Each has a read-only tool, and each failure has a
different answer:

| Check | Tool | If it fails |
|---|---|---|
| Compute is enabled for the project | `compute_locations_list` | Nothing can be placed. Datum's to enable — the user runs `datumctl compute access request`, and approval is a manual step on Datum's side. |
| Somewhere to run it | `compute_locations_list` | The city codes it returns are the only ones a placement may name; they come from compute's own availability records, so a city missing from the list is one compute is not offered in. An empty list means nothing is available to this project yet; that is Datum's, not something the user can add. |
| A network | `compute_networks_list` | `default` by convention. If it is missing, `compute_workload_plan` says so and `compute_workload_apply` creates it alongside the workload — say so when you show the plan, because it is a second object being created. |
| Quota | `compute_quota_get` | Quota is granted by Datum and cannot be self-served. A project with none can still create a workload; its instances then sit at `QuotaGranted=False` with `QuotaNoBudget` and never start. |

Do the quota arithmetic before you apply, not after. Replicas times the instance
type against what `compute_quota_get` says is left tells you whether this will start.
If it will not, say so *before* asking for confirmation — a workload that
creates cleanly and then sits at `QuotaExceeded` looks like a success and is
not. Load `quota-triage` for the difference between being over quota and having
none.

## 2. Container or virtual machine

There are two runtimes and a workload picks exactly one. This is not adjustable
later — switching means a different workload.

**Container (a sandbox).** The common case. One or more containers, each with a
fully qualified image. Choose this unless the user needs a whole operating
system.

**Virtual machine.** A full OS booted from a disk image. Choose this only if the
user asks for one, or needs to log into the machine. It carries the extra
requirements in the trap list below.

### The image is a prerequisite, not an input you can produce

The image must:

- **already exist in a registry** the platform can reach. You cannot build one.
- **be fully qualified** — `docker.io/netdata/netdata:latest`, not `netdata`.
  A bare name is the most common cause of `ImageUnavailable` afterwards.
- **be built for the runtime Datum runs it on.** An image that runs on a laptop
  can still fail here. The user builds it with `datumctl compute build`, which
  checks for the known incompatibilities and can fix them.

If the user has no image yet, stop and say that: the build is theirs to run, and
everything below waits on it. Do not render a manifest around an image name
nobody has pushed.

## 3. Gather the inputs

Ask for what is missing rather than inventing it. `compute_workload_render` takes:

- **name** — a DNS label (lowercase letters, digits and `-`). It is the object's
  name and cannot be changed later.
- **image** — fully qualified, per above.
- **placements** — one or more city codes from `compute_locations_list`, and the
  replica count for each. Group cities that scale together into one placement.
- **replicas** — `minReplicas` must be at least 1. There is no scaling from
  zero, and the ceiling is 1000.
- **port** — optional, and named. A port is how anything reaches the workload;
  ask whether it serves traffic rather than guessing.
- **environment variables** — literal values, or drawn from a ConfigMap or a
  Secret.
- **ConfigMap and Secret references** — mounted as volumes, or read as
  environment variables. They must already exist in the project, and the user
  must be able to read them, or create is rejected.
- **a public IPv4 address** — only if the workload has to be reachable from the
  internet on IPv4. Ask; do not add one by default, and do not leave it out of a
  workload that clearly needs one, because it cannot be added afterwards.

## 4. The traps

These are the ones that cost a round trip. Check the rendered manifest against
this list before you validate.

1. **One instance type.** `datumcloud/d1-standard-2` is the only one accepted
   today. `compute_instance_types_list` is the check; anything else is rejected outright.

2. **Per-container CPU and memory are not accepted.** A `resources` block on a
   container is rejected, and so are adjustments to the instance type's own
   requests. The size of an instance comes from the instance type and nothing
   else. If the user wants a different size, that is a request to Datum.

3. **ConfigMap volumes use `name`; Secret volumes use `secretName`.** The two
   spellings sit next to each other in the same list and are not
   interchangeable. Getting it wrong reads as a missing required field.

4. **Every volume must be attached.** A volume that is declared and never
   attached to a container or to the virtual machine is rejected — the create
   fails on the volume, not on the attachment.

5. **The network interface is settled at create.** Its name, the address
   families it carries, any extra addresses (a public IPv4 among them), and what
   becomes of those addresses when the instance goes away are all immutable. An
   instance gets one interface. If any of this turns out to be wrong later, the
   fix is a new workload, so ask now:
   - IPv6 only is the default. If the workload has to answer on IPv4, that has
     to be asked for at create.
   - A published address — one in DNS, or allowed through someone's firewall —
     wants a reclaim policy that keeps it, and that choice is also final.

6. **Virtual machines need two extra things.** SSH keys on the template's
   metadata, under the annotation `compute.datumapis.com/ssh-keys`, one
   `username:key` line per key — a create without them is rejected. And the
   first volume attached must be a bootable disk populated by an Ubuntu image.
   First, not merely present.

7. **ConfigMaps and Secrets have size limits**: 256 KiB per object, and 1 MiB
   for everything one workload references put together. Over either and the
   workload reports `SourceTooLarge` rather than failing at create.

8. **Editing a ConfigMap does not restart anything.** The new contents reach the
   machines, but a process that read the file at startup goes on running with
   what it read. Say this whenever a config change is the point of the
   conversation — the user has to restart the workload themselves, and there is
   no tool here that does it.

## 5. The sequence

Follow it in order. Each step exists because of a failure the next one cannot
catch.

1. **`compute_workload_render`** — inputs in, a full manifest out. It writes nothing and
   reaches nothing. Read what came back rather than assuming it matches what you
   asked for.

2. **`compute_workload_validate`** — the server checks the manifest without creating
   anything. This is where the traps above surface as real rejections, and it is
   also where you learn whether a workload of this name already exists: for an
   existing one, validate returns the diff instead.

3. **Show the user the manifest and, if there is one, the diff.** Whole, not
   summarised. Then say in plain words what will be created, where, how many,
   and what it will cost against their quota. If the plan says the network has
   to be created too, say that: it is a second object.

4. **`compute_workload_plan`** — validates again, settles whether this is a create or an
   update, says whether the network has to be created too, and mints the token
   over the manifest it returns. Show that manifest, not your own draft.

5. **Get an explicit yes.** A question about the plan is not a yes. "Looks
   right" is. If the user asks for any change, go back to step 1 — a token
   minted for the old manifest is not valid for the new one, and must not be
   applied because it was close.

6. **`compute_workload_apply`** with the plan's manifest and its token.

7. **`compute_workload_diagnose`** for the rollout. Creation succeeding means the
   request was accepted, not that anything is running. Tell the user what to
   expect: instances appear, then start, and the first pull of a large image
   takes a while. If it is not serving, that is `workload-not-available`'s
   procedure, not this one.

## What to do when a step fails

- **Render is missing something** — an input you did not gather. Ask for it by
  name. Do not fill it in with a plausible default; a guessed port or city is a
  workload that runs in the wrong place.

- **Validate rejects it** — this is the server's own answer, in its own words,
  and it names the exact field. Quote the field path verbatim and translate the
  rule beside it: `spec.template.spec.volumes[1].name: volume must be attached
  at least 1 time` is "the `config` volume is declared but never mounted". Fix
  it, render again, validate again. Never apply something that failed validate.

- **Validate returns a diff you did not expect** — a workload of that name is
  already there. Stop and say so. Ask whether the user meant to change the
  existing one, and check the diff for anything immutable from trap 5 before
  going on, because those rejections arrive at apply and not before.

- **Plan fails** — the manifest was rejected on the second look, or the
  workload moved underneath you between validate and plan. A failed plan mints
  no token, so there is nothing to apply. Re-read, re-render, and show the user
  again. Do not retry a plan you do not understand the failure of.

- **Apply refuses the token** — something changed after the plan. That refusal
  is the mechanism working. Re-plan, show the new manifest, and ask again.
  Never work around it.

- **Apply succeeds and nothing starts** — hand it to `compute_workload_diagnose` and
  follow the skill it names. Quota and image problems both look like this and
  lead to opposite advice.

## If the user has a shell

You are usually working without one. When the user is at a terminal, the same
workload is one command, and these are theirs to run, not yours to assume:

    datumctl compute access request
    datumctl compute build --push --output ghcr.io/acme/api:1.4.2 .
    datumctl compute deploy api --image=ghcr.io/acme/api:1.4.2 --city=DFW --min=1 --port=8080

Offer them when a step above has no tool behind it — the access request has
none at all — and otherwise stay with the tools, which is the path that shows
the user the manifest before anything is created.

## Reporting

Say what will exist, where, and how many, in the user's own words first: "one
container running `ghcr.io/acme/api:1.4.2` in Dallas, two replicas, answering on
port 8080". Then the identifiers — the workload name, the image with its tag,
the city codes — because those are what they need to check it themselves or to
escalate.

After apply, say plainly that the workload was created and that it is not
running yet, and what you will look at next. A create reported as a deploy is
the same mistake as reporting a pointer reason: technically true, and it reads
as more than it is.

## When to file a capability gap

`report_capability_gap__compute-datumapis-com` is for cases where these tools
could not get a legitimate creation done:

- A field the user needs that `compute_workload_render` has no input for, where the API
  clearly supports it — `InsufficientDetail`, quoting the field and what you
  tried.
- A validate rejection whose message does not name what to change, so the user
  cannot act on it — `UnactionableGuidance`, quoting the message verbatim.

Not gaps, however awkward the turn:

- **No image.** Building one was never in scope here.
- **No quota, or Compute not enabled.** Those are grants, and the tools
  reporting them accurately is the tools working.
- **A rejection that was right.** An unsupported instance type or an unattached
  volume is validate doing its job — that is the answer, and it saved a broken
  workload.
- **The user declined to confirm.** Not applying is the correct outcome.
