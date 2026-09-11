# flagsmith-testbed

A provider-TCK backend for **Flagsmith**, built on the Flagsmith **Edge Proxy**.

Status: **working prototype, 2026-09-11.** Scratch repo, no permanent home. Every control-API
operation is implemented and verified against a running container; **no OpenFeature adoption exists
yet**, so nothing here has been driven through a provider.

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
  ┌──────────────────────────────────────────────────┐
  │  launchpad (PID 1, Go)                           │
  │    :8080  control API  /start /stop /restart     │
  │                        /change /reset /healthz   │
  │    :8080  GET /api/v1/environment-document/  ◀─┐ │
  │                                                │ │   polls every 1s
  │  edge-proxy (Flagsmith, real flag_engine)      │ │
  │    :8000  GET /api/v1/flags/                 ──┘ │
  │           POST /api/v1/identities/               │
  └──────────────────────────────────────────────────┘
        ▲
        │  provider under test
```

One image, two processes. The launchpad supervises the proxy, which keeps `POST /stop` meaning
*kill the backend process* rather than *stop the container* — the control API requires that,
because dynamically mapped host ports do not survive a container restart.

No Postgres, no Django, no Flagsmith API. See `../flagsmith-tck-plan.md` §4 for what that costs:
this route does **not** prove the control API survives a database-backed vendor stack, which was
the full stack's whole point.

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

Or `docker compose up --build`, which maps both ports dynamically the way the TCK expects.

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
| `GET /healthz` | launchpad liveness | 200 |

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

Every flag is seeded `enabled: true` and booleans are modelled as values, not as Flagsmith's
enabled state — a disabled flag resolves to the *caller's default* (FINDINGS #6), which would
silently defeat the falsy-value scenarios.

Observed through the real engine:

```
boolean-flag         true          integer-flag         10
boolean-zero-flag    false         integer-zero-flag    0
string-flag          "hi"          large-integer-flag   2147483647
string-zero-flag     ""            huge-integer-flag    9007199254740991
float-flag           "0.5"         integral-float-flag  "10.0"
wrong-flag           "uno"         changing-flag        "foo"
object-flag          "{\"imagesPerPage\":100,\"showImages\":true,\"title\":\"Check out these pics!\"}"
missing-flag         404
```

Falsy values survive, and 2^53-1 is exact.

## Predicted capability declarations

Predictions, not measurements — no adoption exists.

| | `@events` | `@lifecycle` | `@stale` | `@config-change` | `@object` | `@unavailable` | `@numeric-coercion` |
| --- | :-: | :-: | :-: | :-: | :-: | :-: | :-: |
| Go, default mode | ? | ? | ? | ? | yes | ? | n/a (#9, #2) |
| Go, `WithUsingBooleanConfigValue` | ? | ? | ? | ? | yes | ? | n/a |

The `?`s are FINDINGS #10: nothing is known about whether any Flagsmith provider implements
`StateHandler`/`EventHandler`. Both Go modes must be declared separately — they disagree about what
a boolean flag is (FINDINGS #5).

## Not done

- **No adoption.** The next step is `go-sdk-contrib/tools/provider-tck`, both Go modes.
- **No published image**, so no contrib repo's CI can depend on this and any adoption PR stays a
  draft.
- **Only Go's provider has been read.** Java, JS, PHP and Ruby are hand-written against the same
  API; divergence between them is the highest-value thing an adoption could surface.
- **No `identities` coverage.** The proxy serves `POST /api/v1/identities/` and the providers use it
  whenever a targeting key is present, which is also where FINDINGS #4 bites. Untested here.
- Only the `default` configuration exists.
