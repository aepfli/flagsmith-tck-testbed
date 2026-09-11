# Findings

Pre-registered *before* any adoption runs, so "the suite found things" stays a prediction rather
than a post-hoc story.

Each finding says how it was established. **source** means read from the named file; **runtime**
means observed against the container built from this repo on 2026-09-11. Nothing marked *source*
has been reproduced through an OpenFeature provider yet, because no adoption exists.

---

## 1. The control API cannot communicate connection parameters — *runtime, structural*

`specification/assets/provider-tck/openapi/control-api.yaml` defines `/start /stop /restart
/change /reset /healthz`, and `/start` returns a bare `200` with no body. For flagd, host and port
is the whole story. Flagsmith needs an **environment key**, and so will every other SaaS-shaped
backend — LaunchDarkly, ConfigCat, Split.

Worked around here by pinning fixed keys (`ser.provider-tck-server-key` /
`provider-tck-client-key`) that the adoption hardcodes exactly as it hardcodes a port. That works
only because this testbed controls both the proxy config and the document; a real Flagsmith stack
generates keys and could not do it.

**This is an Appendix F gap, not a Flagsmith problem.** Likely resolution: a `GET /info` returning
vendor-defined connection parameters. Worth filing on the general argument rather than waiting for
a backend that cannot be worked around.

## 2. Go provider: `GetFloatValue` cannot read a Flagsmith integer — *source*

`go-sdk-contrib/providers/flagsmith/pkg/provider.go`. The two numeric accessors disagree about the
wire type:

- `IntEvaluation` asserts `res.Value.(float64)` — it wants a JSON **number**.
- `FloatEvaluation` asserts `res.Value.(string)` and then `strconv.ParseFloat`, commented
  *"Because We store floats as string"*.

So for a Flagsmith integer feature (`feature_state_value: 10`, which is what the backend natively
stores), `GetIntValue` succeeds and `GetFloatValue` returns `TYPE_MISMATCH`. No single seeding
satisfies both accessors. This is reachable through ordinary Flagsmith usage and looks simply
wrong; it also makes the integer→float half of `@numeric-coercion` unsatisfiable.

## 3. Go provider: float→int narrowing, but **latent** — *source*

`IntEvaluation` ends with `int64(value)` on a `float64`, with no check for a fractional part. That
is the same defect class as flagd (returns `0` for a float read as int) and GOFF
(`evaluation.go`'s `case float64: if expectedType == "int"`).

**Do not report this as a third confirmed instance.** Flagsmith has no float type —
`feature_state_value` is natively boolean, integer or string — so the backend cannot produce a
fractional JSON number and the narrowing branch is unreachable through Flagsmith itself. It is a
latent defect in the provider, reachable only if some other document source supplies a float.
Stated precisely because the interesting claim ("every provider narrows floats") is weaker than it
first looks.

## 4. Go provider: `TARGETING_MATCH` is claimed without a match — *source*

`resolveFlag` sets `reason = of.TargetingMatchReason` as soon as a targeting key is present in the
evaluation context, before any evaluation happens, and never revises it. A flag with no targeting
rules — which is *every* flag in the canonical set, deliberately — still reports
`TARGETING_MATCH` if the scenario passed a targeting key.

Direct consequence for the TCK: every scenario asserting `STATIC` fails the moment a targeting key
is in context. This is the single most likely cause of a red first run, and it is a provider bug,
not a testbed one.

## 5. Two modes of the same provider disagree about what a boolean flag *is* — *source*

Flagsmith models a flag as `enabled` (bool) **plus** `feature_state_value`. The provider's default
mode reads booleans from `feature_state_value`; `WithUsingBooleanConfigValue()` instead makes the
value `reason != DISABLED`, i.e. the flag's *enabled state*, ignoring `feature_state_value`
entirely.

Same shape as [go-sdk-contrib#939](https://github.com/open-feature/go-sdk-contrib/issues/939)
(flagd RPC vs in-process disagreeing about `PROVIDER_STALE`): a configuration switch silently
changes evaluation semantics. Both modes need their own capability declaration, and the suite
should run both.

## 6. A disabled flag resolves to the **caller's default**, with reason `DISABLED` — *source*

`resolveFlag` returns `defaultValue` and `of.DisabledReason` when `!flagObj.Enabled`, with no error
code. This is why every flag in this testbed is seeded `enabled: true` and booleans are modelled as
values: seeding `boolean-zero-flag` as `enabled: false` would resolve to whatever default the
scenario passed, not to `false` — and the falsy-value scenarios exist precisely to catch a provider
treating `false` as an absence.

Whether `DISABLED`-with-no-error-code is correct per the spec is worth a separate question. It is
not obviously wrong, but it means a provider can return the caller's default on a perfectly
successful evaluation.

## 7. Edge Proxy readiness is honest — *runtime, negative finding*

Checked deliberately, because [flagd#2047](https://github.com/open-feature/flagd/issues/2047) and
[flagd-testbed#394](https://github.com/open-feature/flagd-testbed/pull/394) were both this bug.

`src/edge_proxy/server.py`'s `lifespan` awaits `refresh_environment_caches()` **before** uvicorn
accepts connections, and `/proxy/health/readiness` returns 503 until `last_updated_at` is set. No
window was observed between the proxy accepting connections and serving flags.

`POST /start` still probes a real evaluation before returning — the requirement is not conditional
on the backend being badly behaved — but the probe is cheap here rather than load-bearing.

## 8. `endpoint_caches` is a dormant repeat-evaluation trap — *source*

`AppSettings.endpoint_caches` defaults to `None`, so caching is off unless configured. When it is
set, `EnvironmentService.__init__` wraps `get_flags_response_data` in an `lru_cache`, and the
repeat-evaluation scenarios would hit it exactly as flagd's RPC LRU does.

Pinned explicitly to `use_cache: false` in the generated config rather than inherited, because the
default is the kind of thing that changes in a minor release.

## 9. Flagsmith has no float and no object type — *source, by design*

`feature_state_value` is natively boolean, integer or string. Floats and objects are stored as
strings and parsed by the provider (`strconv.ParseFloat`, `json.Unmarshal`).

Not a bug — a backend constraint the translation has to honour. Consequences: `@numeric-coercion`
is **not applicable** against this backend (see also #2), and `object-flag` round-trips through a
JSON string.

## 10. Open — needs an adoption to settle

- Do the Java, JS, PHP and Ruby Flagsmith providers share #2, #4 and #5? All five are
  hand-written against the same API, so divergence is likely and is the highest-value thing an
  adoption can surface.
- What does the provider report for `@unavailable`? `/stop` makes the proxy refuse connections;
  the Go provider maps a client error to `GENERAL`, not to a failed `initialize`, so
  `@unavailable` may be unsatisfiable the way it is for GOFF remote.
- Does any Flagsmith provider implement `StateHandler`/`EventHandler` at all? If not, `@events`,
  `@lifecycle`, `@stale` and `@config-change` are all undeclared, and `/change` and `/restart`
  exist here for nothing but future use.
