# flagsmith-tck-testbed

A provider-TCK backend for **Flagsmith**, built on the Flagsmith **Edge Proxy**.

Status: **working prototype, 2026-09-13.** Scratch repo, no permanent home. Image tagged `0.1.0`.

Every control-API operation is implemented and verified against a running container, and **four
language providers have now been driven through it end to end** — all against this one image,
through the same control API, with no testbed changes for any of them. Out of 56 scenarios:

The current numbers are below, under [Current results](#current-results).

Draft PRs: [go-sdk-contrib#959](https://github.com/open-feature/go-sdk-contrib/pull/959) (both
evaluation modes, byte-identical),
[java-sdk-contrib#1849](https://github.com/open-feature/java-sdk-contrib/pull/1849),
[js-sdk-contrib#1623](https://github.com/open-feature/js-sdk-contrib/pull/1623),
[Flagsmith#40](https://github.com/Flagsmith/flagsmith-openfeature-provider-python/pull/40).

See [FINDINGS.md](FINDINGS.md) for what reading the source turned up before any adoption ran.

## The design in one line

> **Fake the config source, never the evaluation.**

The evaluation engine under test is real: the Edge Proxy runs Flagsmith's own `flag_engine` over a
real environment document. Only the *delivery* of that document is synthetic — the launchpad serves
`GET /api/v1/environment-document/` in place of the Flagsmith API.

That is the same thing flagd-testbed already does (real flagd, backed by a JSON file the launchpad
rewrites) and what goff-testbed does (real relay proxy, file retriever). Three testbeds, one shape.

It is emphatically **not** stubbing the surface the provider under test talks to. That would make
every assertion an assertion about our own beliefs, and every finding the suite has produced so far
— flagd#2047's readiness window, flagd narrowing a float to `0`, GOFF's `int(value)`,
python-sdk#619 — is real-backend semantics a stub would have answered correctly and passed.

## Architecture

```
                        container
  ┌────────────────────────────────────────────────────────┐
  │  launchpad (PID 1, Go)                                 │
  │    :8080  control API  /start /stop /restart           │
  │                        /change /reset /healthz         │
  │    :8080  GET /api/v1/environment-document/  ◀──────┐  │
  │                                                     │  │  polls every 1s
  │  edge-proxy (Flagsmith, real flag_engine)           │  │
  │    :8000  GET  /api/v1/flags/                  ─────┘  │
  │           GET/POST /api/v1/identities/                 │
  │           GET  /api/v1/environment-document            │
  │           GET  /proxy/health{,/liveness,/readiness}    │
  └────────────────────────────────────────────────────────┘
        ▲
        │  provider under test
```

One image, two processes. The launchpad supervises the proxy, which keeps `POST /stop` meaning
*kill the backend process* rather than *stop the container* — the control API requires that,
because dynamically mapped host ports do not survive a container restart.

No Postgres, no Django, no Flagsmith API. That is deliberate, and it has a cost worth stating
plainly: this route does **not** prove the control API survives a database-backed vendor stack.
Most commercial vendors look like that, so being the first testbed to demonstrate it was the whole
point of running the full Flagsmith stack — and the Edge Proxy buys its small footprint by giving
that up. It becomes the third instance of the flagd pattern rather than the first instance of a new
one.

## Run it

```bash
docker build -t flagsmith-testbed:dev .
docker run -d --name fstb -p 18000:8000 -p 18080:8080 flagsmith-testbed:dev
```

The container boots into the `default` configuration, so it is usable without an explicit `/start`.

```bash
# evaluate, exactly as a provider does
curl -H "X-Environment-Key: ser.provider-tck-server-key" \
     "http://localhost:18000/api/v1/flags/?feature=boolean-flag"

# control API
curl -X POST "http://localhost:18080/start?config=default"
curl -X POST  http://localhost:18080/change
```

Or via compose, which maps both ports dynamically the way the TCK expects — never pinned, so
parallel runs do not collide:

```bash
docker compose up -d --build
docker compose port flagsmith 8000   # -> 0.0.0.0:33674  (provider talks here)
docker compose port flagsmith 8080   # -> 0.0.0.0:33675  (control API)
```

The compose healthcheck polls the launchpad's `/healthz`, so `docker compose ps` reports `healthy`
only once the control API is answering.

## Endpoints

All verified 2026-09-11 through compose-mapped ports.

### Edge Proxy — what the provider under test talks to

| Endpoint | Status | Note |
| --- | :-: | --- |
| `GET /api/v1/flags/` | 200 | all 18 canonical flags |
| `GET /api/v1/flags/?feature=<key>` | 200 | |
| `GET /api/v1/flags/?feature=missing-flag` | 404 | what `FLAG_NOT_FOUND` rests on |
| `GET /api/v1/identities/?identifier=<id>` | 200 | 18 flags + traits; carries targeting, see below |
| `POST /api/v1/identities/` | 200 | body `{identifier, traits[]}` |
| `GET /api/v1/environment-document` | 200 | **local-evaluation mode**, see below |
| `GET /proxy/health` · `/liveness` · `/readiness` | 200 | |
| `GET /health` | 200 | deprecated alias |

Both key kinds are accepted: `ser.provider-tck-server-key` and `provider-tck-client-key`. They
return the same payload here because no feature is server-key-only and `hide_disabled_flags` is
false — but a client key would filter both, so adoptions should use the server key.

### Launchpad — control API

| Endpoint | Status |
| --- | :-: |
| `POST /start?config=default` | 200 |
| `POST /stop` · `/restart?seconds=` · `/change` · `/reset` | 200 |
| `GET /healthz` | 200 |
| `POST /start?config=nope` | 400 |

## Two adoption modes, one testbed

The proxy serves a complete environment document, not just evaluated flags, so both Flagsmith
evaluation modes can point here: **remote**, where the proxy's Python engine evaluates, and
**local**, where the provider's own language engine does. Why that comparison is worth making, and
what it found, is [FINDINGS #12](FINDINGS.md).

**Caveat:** SDKs request `/api/v1/environment-document/` *with* a trailing slash; FastAPI answers
`307` to the slashless route, so the client must follow redirects. Verified: `307 -> 200`.

## Connection parameters for an adoption

The control API has no way to communicate these (FINDINGS #1), so they are fixed and hardcoded:

| | |
| --- | --- |
| Evaluation URL | `http://<host>:<mapped 8000>/api/v1/` |
| Server-side key | `ser.provider-tck-server-key` |
| Client-side key | `provider-tck-client-key` |
| Control API | `http://<host>:<mapped 8080>` |

## Control API mapping

| Op | Implementation | Verified |
| --- | --- | --- |
| `POST /start?config=` | rebuild the document from the canonical set, ensure the proxy is running, **probe a real evaluation before returning** | 992ms |
| `POST /stop` | kill the proxy process; container and control API stay up | evaluation refused, container alive |
| `POST /restart?seconds=` | kill, sleep, restart, probe; **flag state preserved** | 2671ms at `seconds=2` |
| `POST /change` | flip `changing-flag`, bump `updated_at`, poll until the new value is served | 472–987ms |
| `POST /reset` | restore the baseline document, **no process restart** | 994ms |
| `GET /healthz` | **control API** readiness, not the backend's | 200 |

### `/healthz` is not a backend readiness probe, deliberately

It reports whether the **control API** is ready, which the OpenAPI document is explicit about: the
backend is deliberately unhealthy during outage scenarios while the control API has to stay
reachable, or the TCK could never end the outage. So `/healthz` answers 200 while the Edge Proxy is
still starting, and evaluating straight after it is a race.

Do not "fix" this by gating it on the backend. The endpoint that carries the readiness guarantee is
`POST /start`, which must not return until the seeded flag state is actually being served. This
repo's own CI failed on that distinction before it called `/start`.

`/change` and `/reset` are bounded by the proxy's 1s poll (`api_poll_frequency_seconds`, lowered
from its default of 10 — nothing would complete in a sane time otherwise).

### `/start` really does reset state

flagd-testbed's `/change` rewrites the raw input file, so a toggle survives a later `/start` and
state leaks across scenarios. Here `/start` rebuilds the document from the canonical source, so the
leak is structurally impossible. Verified: `/change` → `bar`, `/start` → `foo`.

`/reset` is implemented, not deferred. This is a polling architecture on two hops, so a `/start`
blip is expensive and hard for a provider to tell apart from a real event — the same argument that
put `/reset` in goff-testbed's first cut.

## Flag translation

`flags/canonical-flags.json` is a verbatim copy of the spec asset. `launchpad/document.go` turns it
into an environment document. Schema verified against `Flagsmith/flagsmith-go-client@main`
(`flagengine/{environments,projects,organisations,features}/models.go`).

Flagsmith stores `feature_state_value` as **boolean, integer or string only** — no float type, no
object type — so floats and objects are seeded as strings, which is what the providers expect
(FINDINGS #9). The canonical set is decoded with `UseNumber` so `10.0` stays distinguishable from
`10`; without it `integral-float-flag` would be seeded as an integer and the lossless-coercion
scenario would pass without coercing anything.

A flag's `state` maps onto Flagsmith's `enabled`, which is an unusually direct fit: Flagsmith models
a feature state as `enabled` plus `feature_state_value`, exactly the pair `state` and
`defaultVariant` describe.

Everything except the four `disabled-*` flags is enabled, and ordinary booleans are modelled as
values rather than as the enabled state — a disabled flag resolves to the *caller's default*
(FINDINGS #6), which would silently defeat the falsy-value scenarios. The `disabled-*` flags are the
deliberate exception: for them the enabled state is the thing under test, and their configured
values are kept intact so a provider that ignores the state is caught by the value it returns.

Observed through the real engine:

```
boolean-flag         true          integer-flag         10
boolean-zero-flag    false         integer-zero-flag    0
string-flag          "hi"          large-integer-flag   2147483647
string-zero-flag     ""            huge-integer-flag    9007199254740991
float-flag           "0.5"         integral-float-flag  "10.0"
wrong-flag           "uno"         changing-flag        "foo"
targeting-key-flag   "miss"        (-> "hit" for the targeted identity)
object-flag          "{\"imagesPerPage\":100,\"showImages\":true,\"title\":\"Check out these pics!\"}"
missing-flag         404
```

Falsy values survive, and 2^53-1 is exact.

### Targeting is seeded as an identity override

`targeting-key-flag` is the one canonical flag with a rule. Flagsmith's native encoding for it is an
**identity override**, keyed by the targeting key — which *is* a Flagsmith identifier. The reasoning,
and what it verified, is [FINDINGS #15](FINDINGS.md).

## Measured capability declarations

No longer predictions. Identical across both of Go's evaluation modes; the other three run remote
only. A declared capability whose scenarios fail is the preferred shape, so several `yes` entries
below are capabilities the provider gets wrong — the failure is in the results with a deviation
attached rather than hidden as a skip.

| | `@object` | `@large-integers` | `@targeting` | `@disabled-flags` | `@standard-reasons` | `@numeric-coercion` | `@string-typing` | `@variants` | lifecycle & events |
| --- | :-: | :-: | :-: | :-: | :-: | :-: | :-: | :-: | :-: |
| Go | yes | yes | yes | yes | yes¹ | yes¹ | no | no | no |
| Java | yes | refused² | yes | yes | yes¹ | yes¹ | no | no | no |
| Python | yes | yes | yes | yes¹ | — ³ | yes¹ | — ³ | no | no |
| JS | yes | yes | yes | yes¹ | yes¹ | refused⁴ | no | no | no |

¹ declared and failing, with a deviation.
² the Java SDK's integer accessor is 32-bit, so 2^53-1 cannot be asked at all — the TCK refuses it.
³ not shipped in Python's TCK yet.
⁴ JavaScript has one numeric type, so the question is not expressible — the TCK refuses it.

**`@string-typing` is withheld by all four**, and for the same backend reason: Flagsmith has no float
type and no object type, so `float-flag` and `object-flag` genuinely *are* strings here and asking
for them as strings is a correct request rather than a type mismatch. It is a permitted absence, not
a defect, so it carries no deviation anywhere — the capability exists precisely so this is a skip
with a reason instead of a mandatory failure with an excuse.

That does hide one real defect in the JavaScript provider, which stringifies a value of *any* type
rather than only the ones the backend stores as strings. That belongs in an issue against the
provider rather than in the suite's results — see [FINDINGS](FINDINGS.md).

Why each other absence is what it is — a decline, a defect, or something the SDK cannot express — is
[FINDINGS](FINDINGS.md) #5, #10 and #16 rather than repeated here.

## Current results

All four adoptions, 65 scenarios each:

| | Go | Python | Java | JS |
| --- | ---: | ---: | ---: | ---: |
| passed | **43** | 29 | 33 | 31 |
| failed | **2** | 11 | 11 | 11 |
| skipped | 20 | 25 | 21 | 23 |

## Not done

- **No published home.** The image is `ghcr.io/aepfli/flagsmith-tck-testbed`, in a personal
  namespace, which is the single reason #959 stays a draft: a contrib repo's CI should not depend
  on it. This is the next thing to fix.
- **Only Go's provider has been driven through it.** Java, JS, PHP and Ruby are hand-written against
  the same API; divergence between them is the highest-value thing a further adoption could surface.
  FINDINGS #12 says the two Go paths agree exactly; it says nothing about the others.
- **`/change`, `/restart` and `/reset` are observed by nothing.** They work and are verified by CI,
  but the Go adoption declares no event or lifecycle capability, so no scenario drives them. They
  are there for a provider that implements `StateHandler`.
- **No segments.** `project.segments` is empty. Targeting is done with identity overrides, so
  segment evaluation is entirely untested -- fine for the TCK, worth knowing before anyone reuses
  this testbed for something else.
- **Only the `default` configuration exists.**
- **Two scenarios fail and are not solvable here** -- reading `float-flag` or `object-flag` as a
  string succeeds where the suite wants `TYPE_MISMATCH`, because Flagsmith stores both as strings.
  See FINDINGS #11; it needs a decision upstream, not a change in this repo.
