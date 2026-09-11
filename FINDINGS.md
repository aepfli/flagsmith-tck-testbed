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

**Consequence for the TCK: none, as the suite stands.** No scenario in `gherkin/*.feature` passes a
targeting key -- `evaluation.feature` says so explicitly, that flags resolve "to the default variant
with no targeting involved". So the bad branch is never entered and this will not turn a run red.

Recorded anyway, because it is a real provider defect that any application using identity-based
evaluation hits, and because it is exactly what the context-passthrough scenarios would catch once
the control API grows an echo endpoint (see the `@targeting` capability, currently reserved).

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

## 11. Variant is unpopulatable, and 10 scenarios depend on it — *runtime*

The largest single cause of failure in the first real run: 10 of 12 failing scenarios fail with
`variant was ""`.

Flagsmith has no variant concept for a standard feature. A feature state is `enabled` plus
`feature_state_value` and nothing names the value; the Edge Proxy's `map_flag_result_to_response_data`
returns `{feature:{id,name,type}, enabled, feature_state_value}`, with no variant key on the wire at
all. Multivariate features carry `multivariate_feature_option` keys internally, but those are not in
the evaluation response either. So the provider is not dropping the variant -- it never receives one,
and no seeding of the canonical set can produce one.

**This is a finding about the TCK, not about Flagsmith.** The canonical set is expressed in flagd's
format and its own comment says what matters is "the keys, types, variant names and resolved
values". Variant names are not universally available: a backend can be perfectly conformant and have
no such concept. The evaluation scenarios assert variant unconditionally, so any such backend fails
10 scenarios for a reason that is not a defect.

Worth raising on spec#417: either variant assertions need a capability gate, the way `@object` and
`@large-integers` gate theirs, or the canonical set should stop requiring them. Until then the
honest report for Flagsmith is "fails, for a reason the suite cannot currently express".

## 12. The type-mismatch matrix is partly unsatisfiable here — *runtime*

The other 2 failures. Reading `float-flag` as a **string** returns `"0.5"` rather than
`TYPE_MISMATCH`, and `object-flag` as a string returns the raw JSON text.

Neither is a provider bug. Flagsmith has no float type and no object type (#9), so both really are
strings on this backend -- asking for `float-flag` as a string is a correct request that correctly
succeeds. The scenario assumes the backend's type system distinguishes them, and Flagsmith's does
not.

Same shape as #11: a canonical set carrying flagd's type model, asserted against a backend with a
coarser one. Distinguishable from #11 in one respect worth keeping -- this one *could* be fixed by
seeding `float-flag` as something that is not a string, except that #2 means nothing else resolves
through `GetFloatValue`. The two defects lock each other in place.

## 13. Both engines agree exactly — *runtime, negative finding*

Remote and local evaluation produce **byte-identical failure sets**: same 28 passes, same 12
failures, same reasons. The comparison was the main reason for running both modes -- Flagsmith's
engine is reimplemented per language, Python in the Edge Proxy and Go in
`flagsmith-go-client/flagengine` -- and on the canonical set they do not diverge at all.

Recorded because a negative result from a test designed to find divergence is worth as much as a
positive one, and because it will be worth re-running when Java and JS adoptions exist.

## 14. Local evaluation has a startup race the provider cannot close — *runtime*

In local-evaluation mode the client fetches the environment document on a background poll. The
provider implements no `openfeature.StateHandler`, so it has no `Init` in which to block, and the
SDK synthesises `PROVIDER_READY` on registration. Evaluations in that window return the code default
with error code `GENERAL` and the message "local environment has not yet been updated".

This is the flagd#2047 shape moved into the provider: something reports ready before it can serve a
flag. A conformant provider would block in `Init`. The adoption cannot fix it and waits for the
first sync before handing the provider back, which is why the suite is deterministic rather than
flaky -- the underlying defect is unchanged.

## 15. `updated_at` must carry a timezone, and only one consumer enforces it — *runtime, our bug*

Fixed here, recorded because the failure mode was misleading. The launchpad first emitted
`updated_at` as a naive ISO timestamp. The Edge Proxy's Python accepted it happily
(`datetime.fromisoformat`), so remote evaluation was perfect; the Go engine unmarshals into a
`time.Time`, requires RFC 3339, and failed with `cannot parse "" as "Z07:00"` -- which surfaces as
*every* flag falling back to its code default with `GENERAL`, i.e. as a catastrophically broken
provider rather than as one malformed field.

Django REST Framework emits a timezone, so real Flagsmith would not have hit this. The transferable
part is that the two reference consumers of the same document disagree about how strict the format
is, and only the stricter one tells you.

## 16. Open — still unsettled

- Do the Java, JS, PHP and Ruby Flagsmith providers share #2, #4, #5 and #11? All five are
  hand-written against the same API, so divergence is likely and is the highest-value thing a
  further adoption can surface. #13 says the two Go paths agree; it says nothing about the others.
- **Settled by the Go adoption:** the Go provider implements no `Init`, `Shutdown`, `Status` or
  `EventChannel`, so `@events`, `@lifecycle`, `@stale`, `@configuration-change`, `@unavailable` and
  `@reinitialization` are all undeclared. `/change`, `/restart` and `/reset` are therefore
  implemented here and observed by nothing yet. Whether the other languages' providers do better is
  open.
