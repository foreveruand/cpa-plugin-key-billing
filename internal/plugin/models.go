package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"cpa-key-billing/internal/billing"
)

// modelCatalogKeys are the top-level arrays a model listing uses: OpenAI,
// Claude and Grok wrap entries in "data"; Gemini and the Codex client use
// "models".
var modelCatalogKeys = []string{"data", "models"}

// catalogEntry carries only the fields that identify a listing entry: OpenAI,
// Claude and Grok use "id", the Codex client "slug", and Gemini "name".
type catalogEntry struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// interceptModelList aligns each downstream key's model catalog with the models
// its routing rules allow, and orders the catalog ascending. Every other
// non-streaming response carries a model and is left untouched.
func (a *App) interceptModelList(raw []byte) ([]byte, error) {
	var req ResponseInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("Parse model list interception parameters: %w", errUnmarshal)
	}
	if a == nil || a.store == nil || !a.store.Enabled() {
		return OKEnvelope(ResponseInterceptResponse{})
	}
	// A model listing is the only response without a model; completions fill it.
	if strings.TrimSpace(req.Model) != "" || strings.TrimSpace(req.RequestedModel) != "" || len(req.Body) == 0 {
		return OKEnvelope(ResponseInterceptResponse{})
	}
	decision := billing.RoutingDecision{}
	if scope, ok := callerScopeFromHeaders(req.RequestHeaders); ok {
		decision = a.store.ResolveRouting(scope, "", "")
	}
	curated, changed := curateCatalog(req.Body, decision)
	if !changed {
		return OKEnvelope(ResponseInterceptResponse{})
	}
	return OKEnvelope(ResponseInterceptResponse{Body: curated})
}

// callerScopeFromHeaders recovers the downstream key scope from the inbound
// request. The host passes no Metadata on the model-list path, so the key is
// only available in the credential headers each client API uses.
func callerScopeFromHeaders(headers http.Header) (string, bool) {
	if len(headers) == 0 {
		return "", false
	}
	values := []string{}
	if token := bearerToken(headers); token != "" {
		values = append(values, token)
	}
	for _, name := range []string{"X-Api-Key", "X-Goog-Api-Key"} {
		if headerValues := headers.Values(name); len(headerValues) == 1 {
			values = append(values, strings.TrimSpace(headerValues[0]))
		}
	}
	for _, value := range values {
		if value == "" || len(value) > 8192 {
			continue
		}
		if scope := billing.CallerScope(value); scope != "" {
			return scope, true
		}
	}
	return "", false
}

func bearerToken(headers http.Header) string {
	values := headers.Values("Authorization")
	if len(values) != 1 {
		return ""
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

// curateCatalog filters and re-orders the model array inside a catalog body,
// reporting whether the body changed. Entries keep their original bytes; only
// their array position changes.
func curateCatalog(body []byte, decision billing.RoutingDecision) ([]byte, bool) {
	// A stale bound route refuses every call at request time; leave the catalog
	// alone instead of blanking every client's model picker.
	if decision.ConfigurationError != "" {
		return nil, false
	}
	var root map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil {
		return nil, false
	}
	arrayKey := ""
	for _, candidate := range modelCatalogKeys {
		if _, ok := root[candidate]; ok {
			arrayKey = candidate
			break
		}
	}
	if arrayKey == "" {
		return nil, false
	}
	var entries []json.RawMessage
	if errUnmarshal := json.Unmarshal(root[arrayKey], &entries); errUnmarshal != nil || len(entries) == 0 {
		return nil, false
	}

	restricted := decision.RestrictsModels()
	original := make([]string, len(entries))
	kept := make([]catalogEntryRaw, 0, len(entries))
	for i, entry := range entries {
		var parsed catalogEntry
		if errUnmarshal := json.Unmarshal(entry, &parsed); errUnmarshal != nil {
			return nil, false
		}
		// Every entry must carry an identifier; otherwise leave the body alone
		// rather than order or filter on an empty string.
		sortKey := firstNonEmptyString(parsed.ID, parsed.Slug, parsed.Name)
		if sortKey == "" {
			return nil, false
		}
		original[i] = sortKey
		if restricted && !allowsCatalogEntry(decision, parsed, sortKey) {
			continue
		}
		kept = append(kept, catalogEntryRaw{raw: entry, key: sortKey})
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].key < kept[j].key })

	if len(kept) == len(original) {
		unchanged := true
		for i := range kept {
			if kept[i].key != original[i] {
				unchanged = false
				break
			}
		}
		if unchanged {
			return nil, false
		}
	}

	encoded, errMarshal := marshalNoEscape(catalogRawEntries(kept))
	if errMarshal != nil {
		return nil, false
	}
	root[arrayKey] = encoded
	out, errMarshal := marshalNoEscape(root)
	if errMarshal != nil {
		return nil, false
	}
	return out, true
}

type catalogEntryRaw struct {
	raw json.RawMessage
	key string
}

func catalogRawEntries(entries []catalogEntryRaw) []json.RawMessage {
	out := make([]json.RawMessage, len(entries))
	for i, entry := range entries {
		out[i] = entry.raw
	}
	return out
}

// allowsCatalogEntry reports whether the routing decision permits a listing
// entry. Gemini names arrive as "models/<id>", so both the raw and bare forms
// are matched, keeping a user-written rule independent of the listing format.
func allowsCatalogEntry(decision billing.RoutingDecision, parsed catalogEntry, sortKey string) bool {
	names := catalogEntryNames(parsed, sortKey)
	for _, name := range names {
		if decision.DeniesModelName(name) {
			return false
		}
	}
	if len(decision.Models) == 0 {
		return true
	}
	for _, name := range names {
		if decision.AllowsModelName(name) {
			return true
		}
	}
	return false
}

func catalogEntryNames(parsed catalogEntry, sortKey string) []string {
	names := []string{sortKey}
	// The bare id matters only for the Gemini "name": "models/..." shape, where
	// the sort key came from name rather than id or slug.
	if parsed.ID == "" && parsed.Slug == "" {
		if bare := strings.TrimPrefix(sortKey, "models/"); bare != sortKey && bare != "" {
			names = append(names, bare)
		}
	}
	return names
}

// marshalNoEscape re-encodes without HTML escaping, matching the host's compact
// catalog encoder, and keeps each raw entry's bytes rather than reformatting it.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if errEncode := encoder.Encode(v); errEncode != nil {
		return nil, errEncode
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
