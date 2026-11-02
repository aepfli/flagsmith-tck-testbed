# flagsmith-tck-testbed

A backend for the OpenFeature provider conformance suite (TCK,
[open-feature/spec#417](https://github.com/open-feature/spec/issues/417), Appendix F), standing in
for Flagsmith, built on the Flagsmith **Edge Proxy**. Image `ghcr.io/aepfli/flagsmith-tck-testbed`. Scratch repo, no permanent home yet.

> **Fake the config source, never the evaluation.** The Edge Proxy runs Flagsmith's own
> `flag_engine` over a real environment document; only the *delivery* of that document is synthetic
> — a launchpad serves `GET /api/v1/environment-document/` in place of the Flagsmith API, and the
> TCK's control API alongside it.

## How it works

The launchpad is PID 1 and listens on `:8080`. It serves the TCK control API and
`GET /api/v1/environment-document/`. It supervises the Edge Proxy on `:8000`, which polls that
document in place of the Flagsmith API. Providers talk to the proxy.

## Run it

```bash
docker run -d -P ghcr.io/aepfli/flagsmith-tck-testbed:latest
```

Boots into the `default` configuration, so it serves the canonical set without an explicit `/start`.

```bash
curl -H "X-Environment-Key: ser.provider-tck-server-key" \
     "http://localhost:<8000>/api/v1/flags/?feature=boolean-flag"
curl -X POST "http://localhost:<8080>/change"
```

Adoptions point a compose file at it and let the TCK own the stack. Ports are never pinned.

| | |
| --- | --- |
| Evaluation URL | `http://<host>:<mapped 8000>/api/v1/` |
| Control API | `http://<host>:<mapped 8080>` |
| Server-side key | `ser.provider-tck-server-key` |
| Client-side key | `provider-tck-client-key` |

The keys are fixed because the control API has no way to hand connection parameters to a provider
([FINDINGS.md](FINDINGS.md) #1).

## What it serves

**Edge Proxy** (`:8000`) — `GET /api/v1/flags/`, `GET|POST /api/v1/identities/`,
`GET /api/v1/environment-document`, `/proxy/health{,/liveness,/readiness}`. `missing-flag` 404s,
which is what `FLAG_NOT_FOUND` rests on.

**Launchpad** (`:8080`) — `POST /start /stop /restart /change /reset`, `GET /healthz`.

| Op | Behaviour |
| --- | --- |
| `/start?config=` | rebuild from the canonical set, **probe a real evaluation before returning** |
| `/stop` | kill the proxy; container and control API stay up |
| `/restart?seconds=` | kill, sleep, restart; **flag state preserved** |
| `/change` | flip `changing-flag`, poll until served |
| `/reset` | restore baseline, **no process restart** |

Two things that look like bugs and are not:

- **`/healthz` reports the control API, not the backend.** The backend is deliberately unhealthy
  during an outage scenario while the control API must stay reachable. `POST /start` is the call
  that carries the readiness guarantee.
- **`/start` genuinely resets.** It rebuilds the document from the canonical source, so a `/change`
  cannot leak across scenarios.

## Flag translation

`flags/canonical-flags.json` is a verbatim copy of the spec asset; `launchpad/document.go` turns it
into an environment document (schema verified against `flagsmith-go-client@main`).

`feature_state_value` is natively **boolean, integer or string** — no float type, no object type —
so those are seeded as text, which is what the providers expect. `state` maps onto `enabled`, which
is an unusually direct fit. Two subtleties:

- The canonical set is decoded with `UseNumber`, so `10.0` stays distinct from `10`; otherwise
  `integral-float-flag` would seed as an integer and the lossless-coercion scenario would pass
  without coercing anything.
- Everything except the four `disabled-*` flags is enabled, and ordinary booleans are modelled as
  values — a disabled flag resolves to the *caller's* default, which would silently defeat the
  falsy-value scenarios.

`targeting-key-flag` is seeded as an **identity override**: a targeting key *is* a Flagsmith
identifier. Segments would be wrong — they match on traits, and the canonical rule has none.

## Adopting it

Point the provider's TCK compose file at the image, use the fixed server-side key, and let the TCK
own the stack. The language TCKs show how:
[java-sdk-contrib#1849](https://github.com/open-feature/java-sdk-contrib/pull/1849),
[go-sdk-contrib#959](https://github.com/open-feature/go-sdk-contrib/pull/959),
[js-sdk-contrib#1623](https://github.com/open-feature/js-sdk-contrib/pull/1623),
[Flagsmith#40](https://github.com/Flagsmith/flagsmith-openfeature-provider-python/pull/40).

## Versioning

`version.txt` holds the current version. A `v*` tag publishes `X.Y.Z` and `X.Y`; every push to
`main` publishes `latest` and a `sha-` tag. Pull requests build and smoke-test but never push.

## Limits

- No segments, so segment evaluation is untested.
- No Postgres, no Django, so it does not show the control API surviving a database-backed vendor
  stack.
- `/change`, `/restart` and `/reset` work and CI checks them, but no provider declares an event or
  lifecycle capability, so no scenario drives them.

Findings from running the suite against it: [FINDINGS.md](FINDINGS.md).
