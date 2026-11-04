package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// withCatalog swaps the catalog snapshot for the duration of the test and restores it afterwards.
func withCatalog(t *testing.T, catalogs map[string][]catalogModel) {
	t.Helper()
	modelsMu.Lock()
	oldModels, oldSource := catalogModels, dynamicSource
	catalogModels = catalogs
	dynamicSource = "test"
	modelsMu.Unlock()
	t.Cleanup(func() {
		modelsMu.Lock()
		catalogModels, dynamicSource = oldModels, oldSource
		modelsMu.Unlock()
	})
}

func capabilityByID(t *testing.T, body []byte, id string) map[string]any {
	t.Helper()
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	for _, row := range resp.Data {
		if row["id"] == id {
			return row
		}
	}
	t.Fatalf("model %s missing from /v1/models", id)
	return nil
}

func callModels(t *testing.T) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	return rec.Body.Bytes()
}

// The npm catalog previously only read id/name/credits, dropping capability fields entirely; this locks in the backfill.
func TestParseNPMCatalogRetainsCapabilityFields(t *testing.T) {
	models, err := parseNPMCatalog([]byte(`{"models":[
		{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","credits":"x0.03 credits",
		 "maxInputTokens":1000000,"maxOutputTokens":128000,"maxAllowedSize":1000000,
		 "contextWindow":{"supportedLengths":[1000000,300000],"defaultLength":300000}},
		{"id":"hy3","name":"Hy3","credits":"x0.00 credits","maxInputTokens":192000,"maxOutputTokens":64000}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]catalogModel{}
	for _, m := range models {
		byID[m.ID] = m
	}
	ds := byID["deepseek-v4.1-flash"]
	if ds.MaxInputTokens != 1000000 || ds.MaxOutputTokens != 128000 || ds.MaxAllowedSize != 1000000 {
		t.Fatalf("npm capability fields lost: %+v", ds)
	}
	if ds.ContextDefaultLength != 300000 {
		t.Fatalf("npm default budget lost: %+v", ds)
	}
	// Tiers must be sorted, and the default budget and capability ceiling are two separate fields that must not be merged.
	want := []int{300000, 1000000}
	if len(ds.ContextLengths) != 2 || ds.ContextLengths[0] != want[0] || ds.ContextLengths[1] != want[1] {
		t.Fatalf("npm tiers not normalized: %v", ds.ContextLengths)
	}
	if hy := byID["hy3"]; hy.MaxInputTokens != 192000 || hy.ContextDefaultLength != 0 || len(hy.ContextLengths) != 0 {
		t.Fatalf("models without contextWindow must not get fabricated tiers: %+v", hy)
	}
}

// Production measurement: the live API returns contextWindow (three tiers) and maxAllowedSize; both must be retained.
func TestParseLiveCatalogRetainsContextWindow(t *testing.T) {
	models, _, err := parseLiveCatalog([]byte(`{"models":[
		{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","credits":"x0.11 credits",
		 "maxInputTokens":1000000,"maxOutputTokens":128000,"maxAllowedSize":1000000,
		 "contextWindow":{"defaultLength":300000,"supportedLengths":[300000,600000,1000000]}}
	],"modelPromotions":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Fatalf("models=%d", len(models))
	}
	m := models[0]
	if m.MaxInputTokens != 1000000 || m.MaxOutputTokens != 128000 || m.MaxAllowedSize != 1000000 {
		t.Fatalf("live capability fields lost: %+v", m)
	}
	if m.ContextDefaultLength != 300000 {
		t.Fatalf("wrong default budget: %d", m.ContextDefaultLength)
	}
	if len(m.ContextLengths) != 3 || m.ContextLengths[2] != 1000000 {
		t.Fatalf("three tiers not retained: %v", m.ContextLengths)
	}
}

// On merge, npm only backfills capability fields missing from the live catalog and never overwrites live values.
func TestMergeCatalogsBackfillsCapabilityWithoutOverwriting(t *testing.T) {
	live := []catalogModel{{ID: "a", FromLive: true, MaxInputTokens: 1000000}}
	npm := []catalogModel{{
		ID: "a", MaxInputTokens: 999, MaxOutputTokens: 128000, MaxAllowedSize: 1000000,
		ContextLengths: []int{300000, 1000000}, ContextDefaultLength: 300000,
	}}
	merged := mergeCatalogs(live, npm)
	if len(merged) != 1 {
		t.Fatalf("merged=%d", len(merged))
	}
	got := merged[0]
	if got.MaxInputTokens != 1000000 {
		t.Fatalf("live value overwritten: %d", got.MaxInputTokens)
	}
	if got.MaxOutputTokens != 128000 || got.ContextDefaultLength != 300000 || len(got.ContextLengths) != 2 {
		t.Fatalf("npm did not backfill missing fields: %+v", got)
	}
}

func TestNormalizeContextLengthsFiltersUnusable(t *testing.T) {
	got := normalizeContextLengths([]int{1000000, 300000, 300000, -1, 0, 5000000}, 1000000)
	if len(got) != 2 || got[0] != 300000 || got[1] != 1000000 {
		t.Fatalf("got %v", got)
	}
	if single := normalizeContextLengths([]int{300000}, 1000000); len(single) != 1 {
		t.Fatalf("a single tier should be kept for the caller to judge: %v", single)
	}
}

// When only one site has data, give top-level scalars without per-site details.
func TestModelCapabilitySingleSiteGivesScalars(t *testing.T) {
	withCatalog(t, map[string][]catalogModel{"cn": {{
		ID: "m", MaxInputTokens: 1000000, MaxOutputTokens: 128000,
		ContextLengths: []int{300000, 1000000}, ContextDefaultLength: 300000,
	}}})
	mc, ok := modelCapabilityFor("m")
	if !ok || mc.Ceiling != 1000000 || mc.MaxOutput != 128000 || mc.Default != 300000 {
		t.Fatalf("wrong single-site capability summary: %+v ok=%t", mc, ok)
	}
	if len(mc.BySite) != 0 {
		t.Fatalf("single site should have no per-site details: %+v", mc.BySite)
	}
}

// When site capabilities disagree, never take the maximum: leave the top level empty and give per-site details instead.
func TestModelCapabilityCrossSiteDisagreementOmitsScalars(t *testing.T) {
	withCatalog(t, map[string][]catalogModel{
		"cn":   {{ID: "m", MaxInputTokens: 192000, MaxOutputTokens: 64000}},
		"intl": {{ID: "m", MaxInputTokens: 1000000, MaxOutputTokens: 128000}},
	})
	mc, ok := modelCapabilityFor("m")
	if !ok {
		t.Fatal("capability data should be recognized")
	}
	if mc.Ceiling != 0 || mc.MaxOutput != 0 {
		t.Fatalf("top level must not get a value on disagreement (especially not 1000000): %+v", mc)
	}
	if len(mc.BySite) != 2 || mc.BySite["cn"].Ceiling != 192000 || mc.BySite["intl"].Ceiling != 1000000 {
		t.Fatalf("per-site details missing: %+v", mc.BySite)
	}
}

// When sites agree, give top-level scalars, with the default budget returned separately from the capability ceiling.
func TestHandleModelsEmitsCapabilityFields(t *testing.T) {
	resetModelFilter()
	t.Cleanup(resetModelFilter)
	withCatalog(t, map[string][]catalogModel{"cn": {{
		ID: "deepseek-v4.1-flash", MaxInputTokens: 1000000, MaxOutputTokens: 128000,
		ContextLengths: []int{300000, 600000, 1000000}, ContextDefaultLength: 300000,
	}}})
	row := capabilityByID(t, callModels(t), "deepseek-v4.1-flash")
	if row["context_length"] != float64(1000000) {
		t.Fatalf("context_length should be the capability ceiling: %v", row["context_length"])
	}
	if row["default_context_length"] != float64(300000) {
		t.Fatalf("default budget must be separate from the capability ceiling: %v", row["default_context_length"])
	}
	if row["max_output_tokens"] != float64(128000) {
		t.Fatalf("max_output_tokens=%v", row["max_output_tokens"])
	}
	opts, ok := row["context_length_options"].([]any)
	if !ok || len(opts) != 3 {
		t.Fatalf("tier list=%v", row["context_length_options"])
	}
	if _, exists := row["capabilities_by_site"]; exists {
		t.Fatal("no per-site details needed when sites agree")
	}
}

// Unknown capabilities must omit the fields, never fall back to 1M; upstream request parameters must not be stuffed into the model list either.
func TestHandleModelsOmitsUnknownCapability(t *testing.T) {
	resetModelFilter()
	t.Cleanup(resetModelFilter)
	withCatalog(t, map[string][]catalogModel{"cn": {{ID: "mystery"}}})
	row := capabilityByID(t, callModels(t), "mystery")
	for _, key := range []string{
		"context_length", "max_output_tokens", "default_context_length",
		"context_length_options", "capabilities_by_site", "context_window",
	} {
		if _, exists := row[key]; exists {
			t.Fatalf("unknown capability must not output %s: %v", key, row[key])
		}
	}
}
