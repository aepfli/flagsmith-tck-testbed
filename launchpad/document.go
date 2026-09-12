package main

// Translation of the TCK canonical flag set into a Flagsmith *environment document* --
// the payload a server-side SDK (and the Edge Proxy) fetches from
// GET /api/v1/environment-document/ and evaluates locally.
//
// Schema verified against Flagsmith/flagsmith-go-client@main:
//   flagengine/environments/models.go  EnvironmentModel
//   flagengine/projects/models.go      ProjectModel
//   flagengine/organisations/models.go OrganisationModel
//   flagengine/features/models.go      FeatureStateModel, FeatureModel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

type canonicalFile struct {
	Flags map[string]canonicalFlag `json:"flags"`
}

type canonicalFlag struct {
	State          string                     `json:"state"`
	Variants       map[string]json.RawMessage `json:"variants"`
	DefaultVariant string                     `json:"defaultVariant"`
	Targeting      json.RawMessage            `json:"targeting,omitempty"`
}

// identityOverride is Flagsmith's per-identity feature override, the entry shape of
// environment_document.identity_overrides.
//
// Verified against Flagsmith/flagsmith-go-client@main flagengine/identities/models.go
// (IdentityModel). composite_key is <environment api key>_<identifier>; the engine derives it when
// absent, but the Python side is happier being given it.
type identityOverride struct {
	DjangoID          int             `json:"django_id"`
	Identifier        string          `json:"identifier"`
	EnvironmentAPIKey string          `json:"environment_api_key"`
	CreatedDate       string          `json:"created_date"`
	IdentityUUID      string          `json:"identity_uuid"`
	IdentityTraits    []any           `json:"identity_traits"`
	IdentityFeatures  []*featureState `json:"identity_features"`
	CompositeKey      string          `json:"composite_key"`
}

type feature struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type featureState struct {
	Feature          *feature `json:"feature"`
	Enabled          bool     `json:"enabled"`
	FeatureSegment   any      `json:"feature_segment"`
	DjangoID         int      `json:"django_id"`
	FeatureStateUUID string   `json:"featurestate_uuid"`
	Multivariate     []any    `json:"multivariate_feature_state_values"`
	Value            any      `json:"feature_state_value"`
}

type organisation struct {
	ID               int    `json:"id"`
	Name             string `json:"name"`
	FeatureAnalytics bool   `json:"feature_analytics"`
	StopServingFlags bool   `json:"stop_serving_flags"`
	PersistTraitData bool   `json:"persist_trait_data"`
}

type project struct {
	ID                int           `json:"id"`
	Name              string        `json:"name"`
	HideDisabledFlags bool          `json:"hide_disabled_flags"`
	Organisation      *organisation `json:"organisation"`
	Segments          []any         `json:"segments"`
}

type environmentDocument struct {
	ID                int                 `json:"id"`
	Name              string              `json:"name"`
	APIKey            string              `json:"api_key"`
	Project           *project            `json:"project"`
	FeatureStates     []*featureState     `json:"feature_states"`
	IdentityOverrides []*identityOverride `json:"identity_overrides"`
	UpdatedAt         string              `json:"updated_at"`
}

// translateValue maps a canonical variant value onto what Flagsmith can actually store in
// feature_state_value.
//
// Flagsmith's feature_state_value is natively one of: boolean, integer, string. There is **no
// float type and no object type**. That is a property of the backend, not a shortcut taken here,
// and the Flagsmith providers are written to match it -- the Go provider's FloatEvaluation reads
// the value as a string and calls ParseFloat ("Because We store floats as string"), and its
// ObjectEvaluation json.Unmarshals a string.
//
// So floats and objects are seeded as strings. Integers stay JSON numbers.
//
// The canonical set is decoded with UseNumber so that `10.0` (integral-float-flag) stays
// distinguishable from `10` (integer-flag); without it encoding/json collapses both to float64
// and the lossless-coercion flag would be seeded as an integer, which is exactly the mistake the
// canonical set's own $comment warns about.
func translateValue(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}

	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		return t, nil
	case json.Number:
		s := t.String()
		if strings.ContainsAny(s, ".eE") {
			// A float. Flagsmith has no float type -> store the literal as a string.
			return s, nil
		}
		n, err := t.Int64()
		if err != nil {
			return nil, fmt.Errorf("integer literal %q: %w", s, err)
		}
		return n, nil
	case map[string]any, []any:
		// Objects are stored as a JSON string; the provider unmarshals it back.
		b, err := json.Marshal(t)
		if err != nil {
			return nil, err
		}
		return string(b), nil
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported variant value %T", v)
	}
}

// targetingKeyRule is the one targeting shape the canonical set uses:
//
//	{"if": [{"==": [{"var": "targetingKey"}, "<uuid>"]}, "<variant>", null]}
//
// Parsing it narrowly rather than implementing JsonLogic is deliberate, and the canonical set
// licenses it: the rule "is specified by its behaviour, not by this encoding -- resolve variant
// 'hit' when the evaluation context's targeting key is exactly the uuid below, and 'miss'
// otherwise. Express that however your backend expresses targeting."
//
// Flagsmith expresses it as an **identity override**. A targeting key is a Flagsmith identifier --
// the provider calls GetIdentityFlags(targetingKey) whenever one is present -- so an entry in
// identity_overrides for that identifier, overriding this one feature, is the native encoding.
// Segments would be the wrong tool: they match on traits, and there is no trait here.
//
// A shape this does not recognise is an error rather than a silent skip. Seeding a targeting flag
// with no rule would make the "non-matching context" scenario pass for the wrong reason, and a
// vacuous pass is the failure mode this whole exercise exists to avoid.
func targetingKeyRule(raw json.RawMessage) (identifier, variant string, err error) {
	var rule struct {
		If []json.RawMessage `json:"if"`
	}
	if err := json.Unmarshal(raw, &rule); err != nil {
		return "", "", fmt.Errorf("targeting is not an {\"if\": [...]} rule: %w", err)
	}
	if len(rule.If) != 3 {
		return "", "", fmt.Errorf("targeting \"if\" has %d arms, want 3", len(rule.If))
	}

	// Decoded into a map because the operator key is literally "==", which no struct tag spells.
	var condition map[string][]json.RawMessage
	if err := json.Unmarshal(rule.If[0], &condition); err != nil {
		return "", "", fmt.Errorf("targeting condition is not an object: %w", err)
	}
	eq, ok := condition["=="]
	if !ok || len(eq) != 2 {
		return "", "", fmt.Errorf("targeting condition is not an \"==\" over two operands")
	}

	var varRef struct {
		Var string `json:"var"`
	}
	if err := json.Unmarshal(eq[0], &varRef); err != nil || varRef.Var != "targetingKey" {
		return "", "", fmt.Errorf("targeting condition does not compare \"targetingKey\"")
	}
	if err := json.Unmarshal(eq[1], &identifier); err != nil {
		return "", "", fmt.Errorf("targeting condition's operand is not a string: %w", err)
	}
	if err := json.Unmarshal(rule.If[1], &variant); err != nil {
		return "", "", fmt.Errorf("targeting consequent is not a variant name: %w", err)
	}
	return identifier, variant, nil
}

// buildDocument turns the canonical flag set into an environment document.
//
// A flag's `state` maps onto Flagsmith's `enabled`, and the mapping is unusually direct: Flagsmith
// models a feature state as `enabled` plus `feature_state_value`, which is exactly the pair the
// canonical set's `state` and `defaultVariant` describe.
//
// Everything except the four `disabled-*` flags is ENABLED, and that is load-bearing rather than
// incidental. The Flagsmith providers return the *caller's default* with reason DISABLED for a
// disabled flag, so seeding boolean-zero-flag as `enabled: false` would not resolve to `false` --
// it would resolve to whatever default the scenario passed in, and the falsy-value scenario exists
// precisely to catch that. Ordinary boolean flags are therefore modelled as feature_state_value,
// not as Flagsmith's enabled state. See FINDINGS.md #3.
//
// The `disabled-*` flags are the deliberate exception: they exist to assert the disabled behaviour
// itself, so for them the enabled state IS the thing under test.
func buildDocument(canonicalPath string, apiKey string) (*environmentDocument, error) {
	b, err := os.ReadFile(canonicalPath)
	if err != nil {
		return nil, err
	}
	var cf canonicalFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", canonicalPath, err)
	}

	names := make([]string, 0, len(cf.Flags))
	for name := range cf.Flags {
		names = append(names, name)
	}
	sort.Strings(names) // stable feature ids across runs

	states := make([]*featureState, 0, len(names))
	var overrides []*identityOverride
	for i, name := range names {
		flag := cf.Flags[name]
		raw, ok := flag.Variants[flag.DefaultVariant]
		if !ok {
			return nil, fmt.Errorf("flag %q: defaultVariant %q not in variants", name, flag.DefaultVariant)
		}
		value, err := translateValue(raw)
		if err != nil {
			return nil, fmt.Errorf("flag %q: %w", name, err)
		}
		id := i + 1
		states = append(states, &featureState{
			Feature:          &feature{ID: id, Name: name, Type: "STANDARD"},
			Enabled:          !strings.EqualFold(flag.State, "DISABLED"),
			FeatureSegment:   nil,
			DjangoID:         id,
			FeatureStateUUID: fmt.Sprintf("00000000-0000-0000-0000-%012d", id),
			Multivariate:     []any{},
			Value:            value,
		})

		if len(flag.Targeting) == 0 {
			continue
		}
		identifier, hitVariant, err := targetingKeyRule(flag.Targeting)
		if err != nil {
			return nil, fmt.Errorf("flag %q: %w", name, err)
		}
		hitRaw, ok := flag.Variants[hitVariant]
		if !ok {
			return nil, fmt.Errorf("flag %q: targeted variant %q not in variants", name, hitVariant)
		}
		hitValue, err := translateValue(hitRaw)
		if err != nil {
			return nil, fmt.Errorf("flag %q: targeted variant %q: %w", name, hitVariant, err)
		}
		overrides = append(overrides, &identityOverride{
			DjangoID:          1000 + id,
			Identifier:        identifier,
			EnvironmentAPIKey: apiKey,
			CreatedDate:       timestamp(),
			IdentityUUID:      fmt.Sprintf("00000000-0000-0000-1000-%012d", id),
			IdentityTraits:    []any{},
			CompositeKey:      apiKey + "_" + identifier,
			IdentityFeatures: []*featureState{{
				Feature:          &feature{ID: id, Name: name, Type: "STANDARD"},
				Enabled:          true,
				FeatureSegment:   nil,
				DjangoID:         id,
				FeatureStateUUID: fmt.Sprintf("00000000-0000-1000-0000-%012d", id),
				Multivariate:     []any{},
				Value:            hitValue,
			}},
		})
	}

	return &environmentDocument{
		ID:     1,
		Name:   "provider-tck",
		APIKey: apiKey,
		Project: &project{
			ID:                1,
			Name:              "provider-tck",
			HideDisabledFlags: false,
			Organisation: &organisation{
				ID: 1, Name: "provider-tck",
				FeatureAnalytics: false, StopServingFlags: false, PersistTraitData: false,
			},
			Segments: []any{},
		},
		FeatureStates:     states,
		IdentityOverrides: overrides,
		UpdatedAt:         timestamp(),
	}, nil
}

// timestamp renders updated_at.
//
// It MUST carry a timezone. Django REST Framework emits ISO-8601 with one, and the two consumers
// of this document disagree about whether that is optional: the Edge Proxy's Python parses a naive
// timestamp happily via datetime.fromisoformat, while the Go engine unmarshals into a time.Time
// and rejects anything that is not RFC 3339. A naive timestamp therefore works perfectly in remote
// evaluation and breaks local evaluation with "cannot parse \"\" as \"Z07:00\"" -- which surfaces
// as every flag falling back to its code default with error code GENERAL, and reads like a broken
// provider rather than a malformed document.
func timestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000Z07:00")
}

// setValue replaces one flag's feature_state_value and bumps updated_at, which is what the Edge
// Proxy's If-Modified-Since handling keys off.
func (d *environmentDocument) setValue(name string, value any) bool {
	for _, fs := range d.FeatureStates {
		if fs.Feature.Name == name {
			fs.Value = value
			d.UpdatedAt = timestamp()
			return true
		}
	}
	return false
}

func (d *environmentDocument) value(name string) (any, bool) {
	for _, fs := range d.FeatureStates {
		if fs.Feature.Name == name {
			return fs.Value, true
		}
	}
	return nil, false
}

func (d *environmentDocument) clone() *environmentDocument {
	b, _ := json.Marshal(d)
	var c environmentDocument
	_ = json.Unmarshal(b, &c)
	return &c
}
