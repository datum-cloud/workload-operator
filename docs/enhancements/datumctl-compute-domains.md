# `datumctl compute` — URLs and Domains

**Status:** Draft
**Companion:** [`datumctl-compute-dx.md`](./datumctl-compute-dx.md) (the DX arc this extends), [`../scoping/alb-workload-exposure.md`](../scoping/alb-workload-exposure.md) (technical scoping)

---

## Summary

`datumctl compute deploy --port=8080` runs a container across multiple cities and gives the developer nothing to point a browser at. This proposes closing that gap by adopting the conventions every comparable platform already uses, rather than inventing a Datum-specific vocabulary for it.

Three breaking changes, stated up front:

1. **`--port` becomes `--http-port`.** The name declares the contract: this is an HTTP service, and it gets a URL.
2. **There is no `expose` / `unexpose` verb.** Custom hostnames live under `domains`, matching Heroku, Vercel, and Railway.
3. **The managed URL is automatic and permanent** for any workload that declares an HTTP port. It is not a resource the developer creates or deletes.

Plus one addition that costs almost nothing and that four of five comparable CLIs ship: **`datumctl compute open`**.

---

## What the industry actually does

| Platform | How you declare "this is a web service" | Platform URL | Custom domain | Open in browser |
|---|---|---|---|---|
| **Heroku** | `web:` process type in `Procfile` | automatic | `heroku domains:add` | `heroku open` |
| **Fly.io** | `[http_service]` block in `fly.toml` | automatic | `fly certs add` | `fly open` |
| **Railway** | port detection, then Generate Domain | on request | `railway domain` | `railway open` |
| **Render** | service type: Web Service vs Private Service | automatic | dashboard | — |
| **Cloud Run** | everything is HTTP | automatic | `gcloud run domain-mappings create` | — |
| **Kubernetes** | `Service` + `Ingress`/`Gateway` | none | — | — |

Three patterns hold across all of them, and they are the whole basis of this proposal.

**1. The declared *role* determines exposure — never a port number.** Heroku routes the `web:` process and not `worker:`. Render makes you pick Web Service or Private Service. Fly has an `[http_service]` block whose port field is literally named `internal_port`. Not one of these platforms treats "a port exists" as "publish it."

This is the answer to whether `--port` is intuitive: it isn't, and neither is adding `--public` next to it. `--port` says *what is listening*. Every platform above makes the developer say *what the thing is*. `--http-port` does that in one flag — you cannot type `--http-port 5432` for a Postgres and be surprised by the result, because the flag name states the contract.

**2. The platform URL is free, automatic, and not a managed object.** Nobody runs `heroku expose`. The URL exists because the service is a web service. This kills `expose` and `unexpose` as concepts: to stop serving, you stop declaring an HTTP port, exactly as you would delete an `[http_service]` block on Fly.

**3. "Domain" is the standard word — "expose" is not.** `heroku domains:add`, `vercel domains add`, `railway domain`. The one platform that says "expose" is Kubernetes, where `kubectl expose` defaults to a **ClusterIP** — internal. Borrowing that verb for a public-internet action would be borrowing the least familiar option *and* the one that means the opposite thing.

Datum Compute is general-purpose — sandboxes, VMs, workers, multi-city placement — so it cannot follow Cloud Run and assume everything is HTTP. It sits closest to Fly and Render, and it should follow them: **make the role explicit, then make everything after it automatic.**

---

## Product principles

**1. Declaring an HTTP port is declaring a web service.** One flag, one meaning, no confirmation flag stacked on top of it.

**2. The URL is free, so it is automatic.** A managed URL costs the developer nothing, which is what licenses issuing one without asking. Every platform in the table above makes the same trade.

**3. The URL is the deliverable.** Last line of output, on its own, copy-pasteable.

**4. Never name the machinery.** The developer sees "URL," "backends," "certificate." Never `NetworkService`, `HTTPProxy`, or a condition reason. Per the DX doc, raw platform state stays behind `describe` and `-o yaml`.

**5. Never invent resources that don't exist.** No `endpoint/api created` for a kind nobody can `datumctl get`. Plain language in normal output; `-o yaml` shows the real objects.

**6. One workload, one URL.** Path routing, multiple hostnames, and multi-backend fan-out are manifest territory.

---

## Workflows

### 1. Deploy and get a URL

```
$ datumctl compute deploy api \
    --image=ghcr.io/acme/api:1.4.2 \
    --city=DFW,IAD \
    --min=2 \
    --http-port=8080

Resolving workload "api" in project acme-prod...
  Placement "default": cities=[DFW, IAD], min=2
  HTTP service:        port 8080 → Datum-managed URL

Apply? (Y/n): y
  workload/api created
Saved workload.yaml

Waiting for rollout. Ctrl-C to detach (rollout continues in background).

  PLACEMENT  CITY  UPDATED  READY  OLD  PHASE
  default    DFW         2      0    0  Pending
  default    IAD         2      0    0  Pending
  default    DFW         2      2    0  Done
  default    IAD         2      2    0  Done

Rollout complete in 47s.

Publishing...
  Backends     4 healthy across DFW, IAD
  Edge         programmed
  Certificate  issued

  https://a1b2c3d4.datumproxy.net
```

The URL objects are created alongside the workload, not after the rollout, so backends register as instances come up and the URL is ready within a second or two of the last city reaching `Done`. Creating them afterward would add a visible stall to every deploy.

A workload with no `--http-port` deploys exactly as it does today and says so, so the developer is never left guessing:

```
Rollout complete in 47s.

  No HTTP port declared — this workload is not reachable from the internet.
  To publish it:  datumctl compute deploy api --http-port 8080
```

### 2. Open it

```
$ datumctl compute open api
Opening https://a1b2c3d4.datumproxy.net
```

`--url` prints without opening, for piping. This is `heroku open` / `fly open` / `railway open`, and it is the single most-used command in that family after deploy.

### 3. Find the URL again

There is no `status` command in this CLI today (the DX doc proposes one; it was never built), so
the URL surfaces in the two places a developer already looks — the list view:

```
$ datumctl compute workloads

  NAME     CITIES     READY  IMAGE                     URL
  api      DFW, IAD   4/4    ghcr.io/acme/api:1.4.2    https://api.example.com
  worker   DFW        1/1    ghcr.io/acme/worker:2.0   —
```

and as a field for scripting, so `| jq -r .url` works:

```
$ datumctl compute workloads -o json | jq -r '.[] | select(.name=="api") | .url'
$ datumctl compute open api --url          # the single-workload shortcut
```

When a `status` command does land, the URL belongs in its header — but nothing here should wait on it.

### 4. Diagnose a URL that isn't working

Passing a workload to `domains` gives the per-URL detail view. This is what makes multi-city legible — the thing a developer is paying Datum for and currently cannot see:

```
$ datumctl compute domains api

URL          https://api.example.com
             https://a1b2c3d4.datumproxy.net
Backend      port 8080/tcp

Health       Degraded — 2 of 4 backends healthy

             CITY  BACKENDS  HEALTHY  SERVING
             DFW          2        2  yes
             IAD          2        0  no

  IAD: no healthy backends — instances are running but not passing health checks.
       Traffic is being served from DFW only.

  Next steps:
    Check instances:  datumctl compute instances --workload=api --city=IAD
    Check logs:       datumctl compute logs api --city=IAD
```

Per the DX doc's rule, the CLI renders whatever reason and message the server emits rather than branching on reason strings, so new platform conditions surface without a CLI release.

### 5. Add a custom domain *(phase 2)*

Modeled directly on `heroku domains:add` and `fly certs add`: print the records, then poll.

```
$ datumctl compute domains add api api.example.com

Verifying example.com...

  Add these DNS records:

    TYPE   NAME                    VALUE
    TXT    _datum.example.com      datum-verify=8f3a91c2b7
    CNAME  api.example.com         a1b2c3d4.datumproxy.net

Waiting for DNS. Ctrl-C to detach — run 'datumctl compute domains' to check.

  Domain verified          ✓
  DNS record programmed    ✓
  Certificate issued       ✓

  https://api.example.com
```

```
$ datumctl compute domains

  WORKLOAD  DOMAIN                        STATUS    CERTIFICATE
  api       api.example.com               active    valid
  api       a1b2c3d4.datumproxy.net       active    valid  (Datum-managed)
  web       www.acme.io                   pending   waiting for DNS
```

```
$ datumctl compute domains remove api api.example.com
```

The managed URL always appears in this list and cannot be removed. It is a stable fallback and a useful health-check target.

### 6. Stop serving

There is no `unexpose`, for the same reason Heroku has no such command. Remove the HTTP port:

```
$ datumctl compute deploy api --image=... --city=DFW,IAD --no-http

  HTTP service:  removed — https://a1b2c3d4.datumproxy.net will stop responding
Apply? (Y/n):
```

And `destroy` states the consequence rather than silently dropping it:

```
$ datumctl compute destroy api

Workload:      api
Placements:    1  Cities: DFW, IAD
Min replicas:  2
URLs:          https://api.example.com, https://a1b2c3d4.datumproxy.net

This will delete the workload, all its instances, and its URLs. Continue? (y/N):
```

---

## Command reference

New:

```
datumctl compute open               Open the workload's URL in a browser (--url to print)
datumctl compute domains            List domains across the project
datumctl compute domains <workload> Per-URL detail: hostnames, certificates, per-city backends
datumctl compute domains add        Attach a custom hostname            (phase 2)
datumctl compute domains remove     Detach a custom hostname            (phase 2)
```

Changed:

```
datumctl compute deploy      --port → --http-port (breaking); --no-http removes it
datumctl compute workloads   gains a URL column and a `url` field in json/yaml output
datumctl compute destroy     lists URLs in its summary and deletes them
```

### On breaking `--port`

`--port` is removed, not silently aliased. Silently mapping it to `--http-port` would publish every existing CI workload on the next plugin upgrade — precisely the surprise this design exists to avoid. For one release it remains registered and **errors**:

```
Error: --port has been replaced by --http-port, which publishes the workload on a
public HTTPS URL. Use --http-port 8080 to publish, or --no-http to keep it internal.
```

That is a loud, one-line migration, and the plugin is activation-gated and alpha — the audience for this break is small and reachable.

---

## What this deliberately does not do

- **No path or header routing.** One backend at `/`. Anyone needing `/api` and `/admin` split across workloads writes an `HTTPProxy` manifest.
- **No TCP or UDP exposure.** HTTP/HTTPS only, matching the platform's proxy.
- **No plaintext HTTP.** Always `https://`. No `--insecure`.
- **No backend TLS.** The edge reaches instances over plaintext inside the network. A container terminating TLS itself will not work, and the CLI must say so clearly rather than producing a broken URL.
- **No domain deletion on `destroy`.** A verified domain is a project asset that outlives any one workload.

---

## Phasing

**Phase 1 — the URL.** `--http-port`, automatic managed URL, `open`, URL in `status` / `workloads` / `destroy`, `--url` diagnostics, the `--port` migration error. This is the entire product promise.

**Phase 2 — domains.** `domains` list/add/remove, verification UX, certificate and DNS progress.

**Phase 3 — driven by usage.** Multiple ports, health-check configuration, request metrics on the URL view.

---

## Open questions

1. **What should the managed hostname look like?** `<uid>.datumproxy.net` is stable and collision-free but unmemorable and awkward to share. Heroku moved off `<name>.herokuapp.com` to random-word names for exactly the squatting reason; Fly uses `<app>.fly.dev` and accepts the collision namespace. This is a platform decision the CLI inherits, but it shapes the first-run experience more than anything else in this document, and PR 411 is still a draft — worth pushing on now.

2. **How does `deploy -f workload.yaml` declare an HTTP service?** The manifest path has no `--http-port` equivalent. Fly's answer is that the manifest *is* the declaration (`[http_service]`), which suggests the right answer here is a field on the workload spec rather than CLI-only sugar. That is an API conversation with the compute team, not a CLI one, and until it happens the flag and manifest paths do not converge the way the DX doc claims they do.
