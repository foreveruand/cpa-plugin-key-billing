package plugin

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"cpa-key-billing/internal/billing"
)

func credentialHeader(name, value string) http.Header {
	headers := http.Header{}
	switch name {
	case "bearer":
		headers.Set("Authorization", "Bearer "+value)
	case "x-api-key":
		headers.Set("X-Api-Key", value)
	case "x-goog-api-key":
		headers.Set("X-Goog-Api-Key", value)
	}
	return headers
}

func interceptList(t *testing.T, app *App, headers http.Header, body string) ResponseInterceptResponse {
	t.Helper()
	raw, errHandle := app.HandleMethod(MethodResponseInterceptAfter, mustMarshal(t, ResponseInterceptRequest{
		SourceFormat: "openai", RequestHeaders: headers, Body: []byte(body),
	}))
	if errHandle != nil {
		t.Fatalf("response.intercept_after error = %v", errHandle)
	}
	var response ResponseInterceptResponse
	decodeResult(t, raw, &response)
	return response
}

func catalogIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &parsed); errUnmarshal != nil {
		t.Fatalf("decode curated catalog: %v (body=%s)", errUnmarshal, body)
	}
	ids := make([]string, 0, len(parsed.Data))
	for _, row := range parsed.Data {
		ids = append(ids, row.ID)
	}
	return ids
}

func TestModelListSortsUnrestrictedCatalog(t *testing.T) {
	app := newAppWithPrice(t, true)
	response := interceptList(t, app, credentialHeader("bearer", testAPIKey),
		`{"object":"list","data":[{"id":"b"},{"id":"a"},{"id":"c"}]}`)
	if len(response.Body) == 0 {
		t.Fatal("an unsorted catalog was left untouched")
	}
	if got, want := catalogIDs(t, response.Body), []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	if again := interceptList(t, app, credentialHeader("bearer", testAPIKey),
		`{"object":"list","data":[{"id":"a"},{"id":"b"},{"id":"c"}]}`); len(again.Body) != 0 {
		t.Fatalf("an already-sorted catalog was rewritten: %s", again.Body)
	}
}

func TestModelListKeepsOnlyAllowedModels(t *testing.T) {
	app := restrictApp(t, billing.RouteRule{Models: []string{"a", "c"}})
	response := interceptList(t, app, credentialHeader("bearer", testAPIKey),
		`{"object":"list","data":[{"id":"b"},{"id":"a"},{"id":"c"}]}`)
	if got, want := catalogIDs(t, response.Body), []string{"a", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

func TestModelListRemovesDeniedModels(t *testing.T) {
	app := restrictApp(t, billing.RouteRule{DeniedModels: []string{"b"}})
	response := interceptList(t, app, credentialHeader("bearer", testAPIKey),
		`{"object":"list","data":[{"id":"b"},{"id":"a"},{"id":"c"}]}`)
	if got, want := catalogIDs(t, response.Body), []string{"a", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

func TestModelListSkipsFilteringForTheManagementCatalog(t *testing.T) {
	app := restrictApp(t, billing.RouteRule{Models: []string{"a"}})
	headers := credentialHeader("bearer", testAPIKey)
	headers.Set(ManagementCatalogHeader, "full")
	response := interceptList(t, app, headers, `{"object":"list","data":[{"id":"b"},{"id":"a"}]}`)
	if len(response.Body) != 0 {
		t.Fatalf("a management catalog was curated: %s", response.Body)
	}
}

func TestModelListCanEmptyTheCatalog(t *testing.T) {
	app := restrictApp(t, billing.RouteRule{Models: []string{"z"}})
	response := interceptList(t, app, credentialHeader("bearer", testAPIKey),
		`{"object":"list","data":[{"id":"b"},{"id":"a"}]}`)
	if got := catalogIDs(t, response.Body); len(got) != 0 {
		t.Fatalf("ids = %v, want none", got)
	}
}

// The bare name written in a routing rule must match the Gemini listing's
// "models/<id>" form, and the Codex listing keys on slug rather than id.
func TestModelListHandlesEveryCatalogShape(t *testing.T) {
	app := restrictApp(t, billing.RouteRule{Models: []string{"gemini-2.5-pro", "codex/keep"}})
	t.Run("gemini", func(t *testing.T) {
		response := interceptList(t, app, credentialHeader("bearer", testAPIKey),
			`{"models":[{"name":"models/gemini-3-pro"},{"name":"models/gemini-2.5-pro"}]}`)
		var parsed struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if errUnmarshal := json.Unmarshal(response.Body, &parsed); errUnmarshal != nil {
			t.Fatalf("decode: %v (body=%s)", errUnmarshal, response.Body)
		}
		if len(parsed.Models) != 1 || parsed.Models[0].Name != "models/gemini-2.5-pro" {
			t.Fatalf("models = %+v, want only models/gemini-2.5-pro", parsed.Models)
		}
	})
	t.Run("codex", func(t *testing.T) {
		response := interceptList(t, app, credentialHeader("bearer", testAPIKey),
			`{"models":[{"slug":"codex/drop"},{"slug":"codex/keep"}]}`)
		var parsed struct {
			Models []struct {
				Slug string `json:"slug"`
			} `json:"models"`
		}
		if errUnmarshal := json.Unmarshal(response.Body, &parsed); errUnmarshal != nil {
			t.Fatalf("decode: %v (body=%s)", errUnmarshal, response.Body)
		}
		if len(parsed.Models) != 1 || parsed.Models[0].Slug != "codex/keep" {
			t.Fatalf("models = %+v, want only codex/keep", parsed.Models)
		}
	})
}

func TestModelListLeavesCompletionResponses(t *testing.T) {
	app := newAppWithPrice(t, true)
	raw, errHandle := app.HandleMethod(MethodResponseInterceptAfter, mustMarshal(t, ResponseInterceptRequest{
		SourceFormat: "openai", Model: "gpt-5.5",
		RequestHeaders: credentialHeader("bearer", testAPIKey),
		Body:           []byte(`{"object":"list","data":[{"id":"b"},{"id":"a"}]}`),
	}))
	if errHandle != nil {
		t.Fatalf("response.intercept_after error = %v", errHandle)
	}
	var response ResponseInterceptResponse
	decodeResult(t, raw, &response)
	if len(response.Body) != 0 {
		t.Fatalf("a completion response was rewritten: %s", response.Body)
	}
}

func TestModelListSortsWithoutAKey(t *testing.T) {
	app := newAppWithPrice(t, true)
	response := interceptList(t, app, http.Header{},
		`{"object":"list","data":[{"id":"b"},{"id":"a"}]}`)
	if got, want := catalogIDs(t, response.Body), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

func TestModelListIgnoresUnrecognizedBodies(t *testing.T) {
	app := newAppWithPrice(t, true)
	for _, body := range []string{"not json", `{"choices":[]}`, `{"data":[]}`} {
		if response := interceptList(t, app, credentialHeader("bearer", testAPIKey), body); len(response.Body) != 0 {
			t.Fatalf("body %q was rewritten: %s", body, response.Body)
		}
	}
}

func TestCurateCatalogSkipsConfigurationErrors(t *testing.T) {
	body := []byte(`{"object":"list","data":[{"id":"b"},{"id":"a"}]}`)
	if curated, changed := curateCatalog(body, billing.RoutingDecision{ConfigurationError: "missing route"}); changed {
		t.Fatalf("catalog was curated on a configuration error: %s", curated)
	}
}

func TestCallerScopeFromHeadersReadsEveryCredentialHeader(t *testing.T) {
	for _, name := range []string{"bearer", "x-api-key", "x-goog-api-key"} {
		t.Run(name, func(t *testing.T) {
			scope, ok := callerScopeFromHeaders(credentialHeader(name, testAPIKey))
			if !ok || scope != billing.CallerScope(testAPIKey) {
				t.Fatalf("scope = %q, ok = %v, want %q", scope, ok, billing.CallerScope(testAPIKey))
			}
		})
	}
	if _, ok := callerScopeFromHeaders(http.Header{}); ok {
		t.Fatal("an empty header set produced a scope")
	}
}

// Re-ordering must not reformat an entry: raw bytes, field order, and numeric
// spelling survive, since the host keeps the Codex catalog compact.
func TestModelListPreservesEntryBytes(t *testing.T) {
	app := newAppWithPrice(t, true)
	body := `{"object":"list","data":[{"id":"b","meta":{"ratio":1.0,"tail":0},"zeta":1},{"id":"a","owned_by":"x"}]}`
	response := interceptList(t, app, credentialHeader("bearer", testAPIKey), body)
	var parsed struct {
		Data []json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(response.Body, &parsed); errUnmarshal != nil {
		t.Fatalf("decode: %v (body=%s)", errUnmarshal, response.Body)
	}
	if len(parsed.Data) != 2 {
		t.Fatalf("entries = %d, want 2", len(parsed.Data))
	}
	if string(parsed.Data[0]) != `{"id":"a","owned_by":"x"}` {
		t.Fatalf("entry bytes changed: %s", parsed.Data[0])
	}
	if string(parsed.Data[1]) != `{"id":"b","meta":{"ratio":1.0,"tail":0},"zeta":1}` {
		t.Fatalf("entry bytes changed: %s", parsed.Data[1])
	}
}
