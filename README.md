# flagsmith-tck-testbed

A provider-TCK backend for **Flagsmith**, built on the Flagsmith **Edge Proxy**.

> **Fake the config source, never the evaluation.** The Edge Proxy runs Flagsmith's own
> `flag_engine` over a real environment document; only the *delivery* of that document is synthetic
> — a launchpad serves `GET /api/v1/environment-document/` in place of the Flagsmith API, and the
> TCK's control API alongside it.

Image `ghcr.io/aepfli/flagsmith-tck-testbed:0.1.0`. Scratch repo, no permanent home yet.

## What four providers do with it

65 scenarios each, one image, one control API, no testbed changes for any of them.

| | Go | Python | Java | JS |
| --- | ---: | ---: | ---: | ---: |
| passed | **45** | 29 | 35 | 31 |
| failed | **2** | 11 | 11 | 13 |
| skipped | 18 | 25 | 19 | 21 |

Draft PRs: [go-sdk-contrib#959](https://github.com/open-feature/go-sdk-contrib/pull/959) (both
evaluation modes, byte-identical), [java-sdk-contrib#1849](https://github.com/open-feature/java-sdk-contrib/pull/1849),
[js-sdk-contrib#1623](https://github.com/open-feature/js-sdk-contrib/pull/1623),
[Flagsmith#40](https://github.com/Flagsmith/flagsmith-openfeature-provider-python/pull/40).

### What works, and what does not

✅ works · ❌ defect · ⊘ not asked (capability withheld or refused)

| | Go | Python | Java | JS |
| --- | :-: | :-: | :-: | :-: |
| Boolean / String / Integer resolution | ✅ | ✅ | ✅ | ✅ |
| **Float resolution** | ✅ | ❌ | ❌ | ❌ |
| Object resolution | ✅ | ✅ | ✅ | ✅ |
| Falsy values (`false`, `0`, `""`) | ✅ | ✅ | ✅ | ✅ |
| 2^31−1 exact | ✅ | ✅ | ✅ | ✅ |
| 2^53−1 exact | ✅ | ✅ | ⊘ 32-bit accessor | ✅ |
| `FLAG_NOT_FOUND` | ✅ | ✅ | ✅ | ✅ |
| Type mismatch, non-string | ✅ | ❌ `bool` is an `int` | ✅ | ✅ |
| **Bool/int as String → mismatch** | ✅ | ⊘ | ✅ | ❌ stringifies |
| Float/object as String | ⊘ | ⊘ | ⊘ | ⊘ |
| **Resolution `reason`** | ❌ targeting | ✅ | ❌ null | ❌ always targeting |
| Targeting | ✅ | ✅ | ✅ | ✅ |
| **Disabled flags** | ✅ | ❌ `GENERAL` | ✅ | ❌ `GENERAL` |
| Numeric coercion | ❌ | ❌ | ❌ | ⊘ one numeric type |
| Variants | ⊘ | ⊘ | ⊘ | ⊘ |
| Lifecycle / events | ⊘ | ⊘ | ⊘ | ⊘ |

The ⊘ rows are permitted absences, not gaps: Flagsmith names no variant, stores floats and objects
as text, and no provider implements `StateHandler`. Reasoning for each is in
[FINDINGS.md](FINDINGS.md); the defects are catalogued in
[flagsmith-tck-findings](https://github.com/aepfli/flagsmith-tck-testbed).

**The recurring split is Go+Java against Python+JS**, on two unrelated axes — what a boolean flag
*is*, and what a disabled flag returns.

## Run it

```bash
docker run -d -P ghcr.io/aepfli/flagsmith-tck-testbed:0.1.0
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
(FINDINGS #1).

## What it serves

**Edge Proxy** (`:8000`) — `GET /api/v1/flags/`, `GET|POST /api/v1/identities/`,
`GET /api/v1/environment-document`, `/proxy/health{,/liveness,/readiness}`. `missing-flag` 404s,
which is what `FLAG_NOT_FOUND` rests on.

**Launchpad** (`:8080`) — `POST /start /stop /restart /change /reset`, `GET /healthz`.

| Op | Behaviour | Verified |
| --- | --- | --- |
| `/start?config=` | rebuild from the canonical set, **probe a real evaluation before returning** | 992ms |
| `/stop` | kill the proxy; container and control API stay up | evaluation refused, container alive |
| `/restart?seconds=` | kill, sleep, restart; **flag state preserved** | 2671ms at `seconds=2` |
| `/change` | flip `changing-flag`, poll until served | 472–987ms |
| `/reset` | restore baseline, **no process restart** | 994ms |

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

## Not done

- **No published home.** The image sits in a personal namespace, which is the one reason all four
  PRs are drafts.
- **Only Go runs both evaluation modes.** Its byte-identical remote/local result rests on Go alone.
- **`/change`, `/restart` and `/reset` are observed by nothing** — no provider declares an event or
  lifecycle capability. They work, and CI proves it, but no scenario drives them.
- **No segments**, so segment evaluation is entirely untested. Fine for the TCK.
- **No Postgres, no Django**, so this does not prove the control API survives a database-backed
  vendor stack — which was the full Flagsmith stack's unique contribution.
