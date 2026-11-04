package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// Model catalog and multipliers
//
// List sources (two-way merge with dedup):
//  1. Live API GET {Base}/v2/enterprises/personal/models (authoritative, includes promos)
//  2. Static catalog inside the npm package: product.internal.json / product.cloudhosted.json
//
// If either source fails, fall back to the local cache; with no cache, use the
// other source's successful data and wait for the next refresh to overwrite it;
// if both fail and there is no cache, show no model list (model calls are
// unaffected; calls are always passed through).
//
// Multipliers:
//  1. Prefer the live API's effective multiplier (credits × promo factor)
//  2. If the API reports 0.00x but the promo has expired, or the model is not
//     in the API catalog, probe once with a same-site account whose balance
//     is not exhausted (one round every 12 hours)
//     - Probe says free → display 0.00x and keep re-checking every 12 hours
//     - Probe says paid → display [Unknown multiplier] until the API reports
//     0.00x again (no promo schedule, or still inside the promo window),
//     which restores Free
//     - 14018 / Unknown → do not override the API value
// -----------------------------------------------------------------------------

const (
	modelsCacheFile   = "wb-models-cache.json"
	modelsCacheSchema = 5
	modelsMinCount    = 1
	modelsMaxCount    = 500
	modelsPath        = "/v2/enterprises/personal/models"

	npmCatalogMaxBytes = 4 << 20
	npmTarballMaxBytes = 80 << 20

	// modelAPIPriceProbeInterval is the price-probe interval for a given site+model.
	modelAPIPriceProbeInterval = 12 * time.Hour
	// Price-probe scheduling: first round shortly after startup; catch up with a
	// short interval while models remain pending; long interval once converged.
	modelPriceProbeStartDelay   = 20 * time.Second
	modelPriceProbeTick         = 30 * time.Minute
	modelPriceProbeCatchUp      = 2 * time.Minute
	modelPriceProbeBatch        = 5
	modelPriceProbeInitialBatch = 30
)

var catalogSites = []string{"cn", "intl", "codebuddy"}

// Per-site catalog files inside the npm package.
var npmCatalogFiles = map[string]string{
	"cn":        "product.internal.json",
	"intl":      "product.cloudhosted.json",
	"codebuddy": "product.json",
}

var npmBases = []string{
	"https://registry.npmjs.org/@tencent-ai/codebuddy-code",
	"https://registry.npmmirror.com/@tencent-ai/codebuddy-code",
}

type catalogModel struct {
	ID              string  `json:"id"`
	Name            string  `json:"name,omitempty"`
	Credits         string  `json:"credits,omitempty"`
	BaseMultiplier  float64 `json:"baseMultiplier,omitempty"`
	Multiplier      float64 `json:"multiplier,omitempty"` // Effective multiplier (including promos)
	HasMultiplier   bool    `json:"hasMultiplier,omitempty"`
	PromoLabel      string  `json:"promoLabel,omitempty"`
	PromoUntil      string  `json:"promoUntil,omitempty"`
	PromoFree       bool    `json:"promoFree,omitempty"`
	PromoExpired    bool    `json:"promoExpired,omitempty"` // Has a promo but it has expired / not started
	FromLive        bool    `json:"fromLive,omitempty"`     // Whether it came from the live API
	MaxInputTokens  int     `json:"maxInputTokens,omitempty"`
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
	MaxAllowedSize  int     `json:"maxAllowedSize,omitempty"` // Maximum allowed context declared upstream
	// ContextLengths / ContextDefaultLength come from the upstream contextWindow:
	// the former is the client-selectable context tiers, the latter the client's
	// default budget. Their semantics differ from the "model capability ceiling"
	// (MaxInputTokens) and must not be merged into one value: the ceiling decides
	// how much input the upstream accepts, while the default budget only decides
	// from how many tokens the client itself starts compressing.
	ContextLengths       []int `json:"contextLengths,omitempty"`
	ContextDefaultLength int   `json:"contextDefaultLength,omitempty"`
}

// modelPriceProbe is the price-probe state for a model on a given site.
type modelPriceProbe struct {
	LastProbeAt int64   `json:"lastProbeAt,omitempty"`
	Verdict     string  `json:"verdict,omitempty"` // free | paid
	Credit      float64 `json:"credit,omitempty"`
	Tokens      int64   `json:"tokens,omitempty"`
	Detail      string  `json:"detail,omitempty"`
}

type modelsCache struct {
	Schema    int                        `json:"schema"`
	Source    string                     `json:"source"`
	FetchedAt int64                      `json:"fetchedAt"`
	Catalogs  map[string][]catalogModel  `json:"catalogs"`
	Probes    map[string]modelPriceProbe `json:"probes,omitempty"`
}

var (
	modelsMu          sync.RWMutex
	catalogModels     = map[string][]catalogModel{}
	modelProbes       = map[string]modelPriceProbe{}
	dynamicSource     string
	modelsScanTrigger = make(chan struct{}, 1)
	modelProbeTrigger = make(chan struct{}, 1)
)

// rateLogState tracks the "official multiplier" log signatures already written by this process.
// The first refresh records everything; afterwards only changed models are logged,
// so the whole catalog is not rewritten into the ordinary log every hour.
var rateLogState = struct {
	sync.Mutex
	seen map[string]string
}{seen: map[string]string{}}

// logOfficialRates writes the multipliers fetched from the official catalog into
// the ordinary log, letting the web console show data source and update time.
// Only summary fields are recorded; no credentials are involved.
func logOfficialRates(site string, models []catalogModel, refreshedAt time.Time) {
	rateLogState.Lock()
	defer rateLogState.Unlock()
	changed := 0
	for _, m := range models {
		key := site + "|" + m.ID
		signature := fmt.Sprintf("%s|%t|%g|%t|%s|%s", m.Credits, m.HasMultiplier, m.Multiplier, m.PromoExpired, m.PromoLabel, m.PromoUntil)
		if previous, ok := rateLogState.seen[key]; ok && previous == signature {
			continue
		}
		rateLogState.seen[key] = signature
		changed++
		source := "npm/cache"
		if m.FromLive {
			source = "live API"
		}
		log.Printf("[Models] official multiplier site=%s model=%s rawCredits=%q effectiveMultiplier=%s hasMultiplier=%t promo=%q promoEnds=%q promoExpired=%t source=%s refreshedAt=%s",
			site, m.ID, m.Credits, fmt.Sprintf("%.2f", m.Multiplier), m.HasMultiplier, m.PromoLabel, m.PromoUntil, m.PromoExpired, source, refreshedAt.Format("2006-01-02T15:04:05"))
	}
	if changed > 0 {
		log.Printf("[Models] Official multipliers recorded: site=%s added or changed this round=%d note=unchanged models are not logged again", site, changed)
	}
}

func probeKey(site, model string) string {
	return site + "|" + normalizeModelName(model)
}

// parseCredits parses the credits field into a numeric multiplier, e.g. "x0.29 credits" -> 0.29.
func parseCredits(v any) (float64, bool) {
	switch x := v.(type) {
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		s = strings.TrimPrefix(s, "x")
		s = strings.ReplaceAll(s, "credits", "")
		s = strings.ReplaceAll(s, "credit", "")
		s = strings.TrimSpace(s)
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
	}
	return 0, false
}

// -----------------------------------------------------------------------------
// Live API
// -----------------------------------------------------------------------------

type livePromotion struct {
	ID       string   `json:"id"`
	Enabled  bool     `json:"enabled"`
	ModelIDs []string `json:"modelIds"`
	Badge    struct {
		Label string `json:"label"`
	} `json:"badge"`
	Discount struct {
		Factor float64 `json:"factor"`
	} `json:"discount"`
	Schedule struct {
		ValidFrom  string `json:"validFrom"`
		ValidUntil string `json:"validUntil"`
	} `json:"schedule"`
}

type liveCatalogData struct {
	Models []struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		Credits         any    `json:"credits"`
		MaxInputTokens  int    `json:"maxInputTokens"`
		MaxOutputTokens int    `json:"maxOutputTokens"`
		MaxAllowedSize  int    `json:"maxAllowedSize"`
		ContextWindow   *struct {
			SupportedLengths []int `json:"supportedLengths"`
			DefaultLength    int   `json:"defaultLength"`
		} `json:"contextWindow"`
	} `json:"models"`
	ModelPromotions []livePromotion `json:"modelPromotions"`
}

func pickCatalogAccount(edition string) *Account {
	accountMu.Lock()
	defer accountMu.Unlock()
	for _, acc := range accounts {
		if acc.Disabled || acc.Auth == nil || acc.Auth.Auth.AccessToken == "" {
			continue
		}
		if profileForEdition(acc.Auth.Edition).Key == edition {
			return acc
		}
	}
	return nil
}

func fetchLiveCatalog(edition string) ([]catalogModel, int, error) {
	acc := pickCatalogAccount(edition)
	if acc == nil {
		return nil, 0, fmt.Errorf("no available %s account", profileForEdition(edition).Label)
	}
	accountMu.Lock()
	auth := *acc.Auth
	accountMu.Unlock()
	return fetchLiveCatalogByAuth(&auth)
}

func fetchLiveCatalogByAuth(auth *StoredAuth) ([]catalogModel, int, error) {
	prof := profileForEdition(auth.Edition)
	headers := func(r *http.Request) {
		commonHeaders(r, prof)
		r.Header.Set("Authorization", "Bearer "+auth.Auth.AccessToken)
		r.Header.Set("X-Client-ID", prof.ClientID)
		r.Header.Set("X-Client-Version", prof.ClientVer)
		r.Header.Set("X-Product", prof.Product)
		if auth.Account.UID != "" {
			r.Header.Set("X-User-Id", auth.Account.UID)
		}
		if auth.Account.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", auth.Account.EnterpriseID)
		}
		if auth.Auth.Domain != "" {
			r.Header.Set("X-Domain", auth.Auth.Domain)
		}
	}
	data, status, err := doJSON(cfg.HttpClient, http.MethodGet, prof.Base+modelsPath, headers, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("HTTP %d: %w", status, err)
	}
	return parseLiveCatalog(data)
}

func promoActive(p livePromotion, now time.Time) bool {
	if !p.Enabled {
		return false
	}
	if p.Schedule.ValidFrom != "" {
		if from, err := time.Parse(time.RFC3339, p.Schedule.ValidFrom); err == nil && now.Before(from) {
			return false
		}
	}
	if p.Schedule.ValidUntil != "" {
		if until, err := time.Parse(time.RFC3339, p.Schedule.ValidUntil); err == nil && !now.Before(until) {
			return false
		}
	}
	return true
}

func parseLiveCatalog(data []byte) ([]catalogModel, int, error) {
	var doc liveCatalogData
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, 0, fmt.Errorf("failed to parse live catalog: %w", err)
	}
	if len(doc.Models) == 0 {
		return nil, 0, errors.New("live catalog models is empty")
	}
	now := time.Now()

	type hit struct {
		factor  float64
		label   string
		until   string
		expired bool
	}
	hits := map[string]hit{}
	active := 0
	for _, p := range doc.ModelPromotions {
		on := promoActive(p, now)
		if on {
			active++
		}
		label := strings.TrimSpace(p.Badge.Label)
		if label == "" {
			label = "Promo"
		}
		for _, id := range p.ModelIDs {
			if on {
				if old, ok := hits[id]; ok && !old.expired && old.factor <= p.Discount.Factor {
					continue
				}
				hits[id] = hit{factor: p.Discount.Factor, label: label, until: p.Schedule.ValidUntil}
			} else {
				// Record "promo exists but not currently active", used to trigger price probing.
				if old, ok := hits[id]; ok && old.expired {
					continue
				}
				hits[id] = hit{label: label, until: p.Schedule.ValidUntil, expired: true}
			}
		}
	}

	seen := map[string]bool{}
	out := make([]catalogModel, 0, len(doc.Models))
	for _, m := range doc.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		entry := catalogModel{
			ID: id, Name: strings.TrimSpace(m.Name), FromLive: true,
			MaxInputTokens: m.MaxInputTokens, MaxOutputTokens: m.MaxOutputTokens,
		}
		var lengths []int
		var defaultLength int
		if m.ContextWindow != nil {
			lengths, defaultLength = m.ContextWindow.SupportedLengths, m.ContextWindow.DefaultLength
		}
		applyCatalogCapability(&entry, m.MaxAllowedSize, lengths, defaultLength)
		if s, ok := m.Credits.(string); ok {
			entry.Credits = strings.TrimSpace(s)
		}
		base, hasBase := parseCredits(m.Credits)
		entry.BaseMultiplier = base
		entry.HasMultiplier = hasBase
		entry.Multiplier = base
		if h, ok := hits[id]; ok {
			entry.PromoLabel = h.label
			entry.PromoUntil = h.until
			if h.expired {
				entry.PromoExpired = true
			} else {
				entry.Multiplier = base * h.factor
				if !hasBase {
					entry.Multiplier = h.factor
				}
				entry.HasMultiplier = true
				if h.factor == 0 {
					entry.PromoFree = true
				}
			}
		}
		out = append(out, entry)
	}
	if len(out) < modelsMinCount || len(out) > modelsMaxCount {
		return nil, 0, fmt.Errorf("abnormal model count: %d", len(out))
	}
	return out, active, nil
}

// -----------------------------------------------------------------------------
// npm static catalog (one of the fallback sources)
// -----------------------------------------------------------------------------

func fetchNPMCatalogVersion() (string, error) {
	traceID := newTraceID()
	log.Printf("[Model catalog] traceId=%s source=npm stage=version query start mirror count=%d", traceID, len(npmBases))
	var lastErr error
	for _, base := range npmBases {
		ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, base+"/latest", nil)
		if reqErr != nil {
			cancel()
			lastErr = reqErr
			log.Printf("[Model catalog] traceId=%s source=npm mirror=%s stage=create request result=failed reason=%v", traceID, base, reqErr)
			continue
		}
		resp, err := cfg.HttpClient.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			log.Printf("[Model catalog] traceId=%s source=npm mirror=%s stage=external request result=failed reason=%v", traceID, base, err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if readErr != nil {
			lastErr = readErr
			log.Printf("[Model catalog] traceId=%s source=npm mirror=%s status code=%d stage=read response result=failed reason=%v", traceID, base, resp.StatusCode, readErr)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			log.Printf("[Model catalog] traceId=%s source=npm mirror=%s result=failed reason=%v", traceID, base, lastErr)
			continue
		}
		var m struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(body, &m); err != nil || strings.TrimSpace(m.Version) == "" {
			lastErr = fmt.Errorf("manifest missing version")
			log.Printf("[Model catalog] traceId=%s source=npm mirror=%s stage=parse version result=failed reason=%v", traceID, base, lastErr)
			continue
		}
		log.Printf("[Model catalog] traceId=%s source=npm result=success version=%s note=response body read and closed, request context then released", traceID, strings.TrimSpace(m.Version))
		return strings.TrimSpace(m.Version), nil
	}
	log.Printf("[Model catalog] traceId=%s source=npm result=query failed reason=%v business impact=existing catalog fallback logic continues to handle it", traceID, lastErr)
	return "", lastErr
}

func parseNPMCatalog(data []byte) ([]catalogModel, error) {
	var doc struct {
		Models *[]struct {
			ID              string `json:"id"`
			Name            string `json:"name"`
			Credits         any    `json:"credits"`
			MaxInputTokens  int    `json:"maxInputTokens"`
			MaxOutputTokens int    `json:"maxOutputTokens"`
			MaxAllowedSize  int    `json:"maxAllowedSize"`
			ContextWindow   *struct {
				SupportedLengths []int `json:"supportedLengths"`
				DefaultLength    int   `json:"defaultLength"`
			} `json:"contextWindow"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("failed to parse npm catalog: %w", err)
	}
	if doc.Models == nil {
		return nil, errors.New("npm catalog is missing the models array")
	}
	seen := map[string]bool{}
	out := make([]catalogModel, 0, len(*doc.Models))
	for _, m := range *doc.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		entry := catalogModel{
			ID: id, Name: strings.TrimSpace(m.Name),
			MaxInputTokens: m.MaxInputTokens, MaxOutputTokens: m.MaxOutputTokens,
		}
		// The npm catalog also carries maxInputTokens/maxOutputTokens/contextWindow;
		// previously only id/name/credits were read and capability info was dropped
		// entirely — fill it in here.
		var lengths []int
		var defaultLength int
		if m.ContextWindow != nil {
			lengths, defaultLength = m.ContextWindow.SupportedLengths, m.ContextWindow.DefaultLength
		}
		applyCatalogCapability(&entry, m.MaxAllowedSize, lengths, defaultLength)
		if s, ok := m.Credits.(string); ok {
			entry.Credits = strings.TrimSpace(s)
		}
		if v, ok := parseCredits(m.Credits); ok {
			entry.BaseMultiplier = v
			entry.Multiplier = v
			entry.HasMultiplier = true
		}
		out = append(out, entry)
	}
	if len(out) < modelsMinCount || len(out) > modelsMaxCount {
		return nil, fmt.Errorf("abnormal npm catalog model count: %d", len(out))
	}
	return out, nil
}

// fetchNPMCatalog fetches a site's catalog: prefers the unpkg single file, falls back to streaming extraction from the npm tgz.
func fetchNPMCatalog(version, file string) ([]catalogModel, string, error) {
	url := "https://unpkg.com/@tencent-ai/codebuddy-code@" + version + "/" + file
	models, err := fetchNPMCatalogJSON(url)
	if err == nil {
		return models, "unpkg", nil
	}
	var lastErr error
	for _, base := range npmBases {
		tgz := base + "/-/codebuddy-code-" + version + ".tgz"
		models, err = fetchNPMCatalogTarball(tgz, file)
		if err == nil {
			return models, "npm-tgz", nil
		}
		lastErr = err
	}
	return nil, "", lastErr
}

func fetchNPMCatalogJSON(url string) ([]catalogModel, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := cfg.HttpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > npmCatalogMaxBytes {
		return nil, fmt.Errorf("catalog response too large: %d bytes", resp.ContentLength)
	}
	data, err := readLimited(resp.Body, npmCatalogMaxBytes)
	if err != nil {
		return nil, err
	}
	return parseNPMCatalog(data)
}

type hardLimitReader struct {
	r         io.Reader
	remaining int64
}

func (r *hardLimitReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, fmt.Errorf("response exceeds the %d bytes limit", npmTarballMaxBytes)
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.r.Read(p)
	r.remaining -= int64(n)
	return n, err
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds the %d bytes limit", limit)
	}
	return data, nil
}

func fetchNPMCatalogTarball(url, file string) ([]catalogModel, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := cfg.HttpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > npmTarballMaxBytes {
		return nil, fmt.Errorf("tgz response exceeds the limit: %d bytes", resp.ContentLength)
	}
	gz, err := gzip.NewReader(&hardLimitReader{r: resp.Body, remaining: npmTarballMaxBytes})
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	want := "package/" + file
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s not found in package", want)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name != want {
			continue
		}
		data, err := readLimited(tr, npmCatalogMaxBytes)
		if err != nil {
			return nil, err
		}
		return parseNPMCatalog(data)
	}
}

// normalizeContextLengths filters invalid tiers and dedups in ascending order.
// Tiers exceeding the model capability ceiling are untrusted (clients filter on
// this too) and are dropped outright.
func normalizeContextLengths(values []int, ceiling int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(values))
	for _, v := range values {
		if v <= 0 || seen[v] {
			continue
		}
		if ceiling > 0 && v > ceiling {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// applyCatalogCapability writes the catalog's capability fields into the entry.
// The capability ceiling prefers maxInputTokens, falling back to the upstream
// declared maxAllowedSize when missing; tiers and the default budget are kept
// only when trusted — a default is never fabricated.
func applyCatalogCapability(entry *catalogModel, maxAllowed int, lengths []int, defaultLength int) {
	if entry == nil {
		return
	}
	if maxAllowed > 0 {
		entry.MaxAllowedSize = maxAllowed
	}
	ceiling := entry.MaxInputTokens
	if ceiling <= 0 {
		ceiling = entry.MaxAllowedSize
	}
	if normalized := normalizeContextLengths(lengths, ceiling); len(normalized) >= 2 {
		entry.ContextLengths = normalized
	}
	if defaultLength > 0 {
		if ceiling > 0 && defaultLength > ceiling {
			defaultLength = ceiling
		}
		entry.ContextDefaultLength = defaultLength
	}
}

// backfillCapability fills missing capability fields only from secondary sources; existing values are never overwritten.
func backfillCapability(dst *catalogModel, src catalogModel) {
	if dst == nil {
		return
	}
	if dst.MaxInputTokens <= 0 {
		dst.MaxInputTokens = src.MaxInputTokens
	}
	if dst.MaxOutputTokens <= 0 {
		dst.MaxOutputTokens = src.MaxOutputTokens
	}
	if dst.MaxAllowedSize <= 0 {
		dst.MaxAllowedSize = src.MaxAllowedSize
	}
	if len(dst.ContextLengths) == 0 {
		dst.ContextLengths = append([]int(nil), src.ContextLengths...)
	}
	if dst.ContextDefaultLength <= 0 {
		dst.ContextDefaultLength = src.ContextDefaultLength
	}
}

// mergeCatalogs merges two catalogs: the live API wins, npm only fills in models the API lacks.
// When a model exists on both sides, npm backfills capability fields missing from
// the live catalog (without overwriting live values).
func mergeCatalogs(live, npm []catalogModel) []catalogModel {
	out := make([]catalogModel, 0, len(live)+len(npm))
	index := map[string]int{}
	for _, m := range live {
		if _, ok := index[m.ID]; ok {
			continue
		}
		index[m.ID] = len(out)
		out = append(out, m)
	}
	for _, m := range npm {
		if i, ok := index[m.ID]; ok {
			backfillCapability(&out[i], m)
			continue
		}
		index[m.ID] = len(out)
		out = append(out, m)
	}
	return out
}

// -----------------------------------------------------------------------------
// Cache
// -----------------------------------------------------------------------------

func loadModelsCache() {
	data, err := os.ReadFile(modelsCacheFile)
	if err != nil {
		return
	}
	var cache modelsCache
	if err := json.Unmarshal(data, &cache); err != nil || cache.Schema != modelsCacheSchema {
		log.Printf("[Models] model cache invalid or schema version too old, ignoring")
		return
	}
	validated := map[string][]catalogModel{}
	for key, models := range cache.Catalogs {
		if len(models) < modelsMinCount || len(models) > modelsMaxCount {
			continue
		}
		validated[key] = models
	}
	modelsMu.Lock()
	if len(validated) > 0 {
		catalogModels = validated
	}
	if len(cache.Probes) > 0 {
		modelProbes = cache.Probes
	}
	dynamicSource = cache.Source
	modelsMu.Unlock()
	for key, models := range validated {
		log.Printf("[Models] loaded site catalog cache: site=%s, model count=%d", key, len(models))
	}
	// Probe verdicts from the cache are also written to the ordinary log: the web
	// console uses them to explain the origin and time of "Paid (unknown multiplier)".
	// Explicitly marked as cache-loaded to avoid being mistaken for a fresh probe this round.
	for key, probe := range cache.Probes {
		if probe.Verdict == "" {
			continue
		}
		site, model, found := strings.Cut(key, "|")
		if !found || site == "" || model == "" {
			continue
		}
		probeTime := "unknown"
		if probe.LastProbeAt > 0 {
			probeTime = time.Unix(probe.LastProbeAt, 0).Format("2006-01-02T15:04:05")
		}
		log.Printf("[ModelPrice] cached verdict site=%s model=%s verdict=%s credit=%s tokens=%d probeTime=%s note=%s source=cache file (not a fresh probe this round)",
			site, model, probe.Verdict, formatQuota(probe.Credit), probe.Tokens, probeTime, probe.Detail)
	}
}

func saveModelsCache(source string, catalogs map[string][]catalogModel, probes map[string]modelPriceProbe) error {
	data, err := json.MarshalIndent(modelsCache{
		Schema: modelsCacheSchema, Source: source, FetchedAt: time.Now().Unix(),
		Catalogs: catalogs, Probes: probes,
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := modelsCacheFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, modelsCacheFile)
}

func persistModelsLocked(source string) error {
	err := saveModelsCache(source, catalogModels, modelProbes)
	if err != nil {
		log.Printf("[Models] failed to write cache: %v", err)
	}
	return err
}

func catalogSnapshot() (map[string][]catalogModel, string) {
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	out := make(map[string][]catalogModel, len(catalogModels))
	for key, models := range catalogModels {
		out[key] = append([]catalogModel(nil), models...)
	}
	return out, dynamicSource
}

func mergedModelIDs() ([]string, string) {
	return collectModelIDs(false)
}

func collectModelIDs(includeDisabled bool) ([]string, string) {
	catalogs, source := catalogSnapshot()
	seen := map[string]bool{}
	ids := make([]string, 0, 64)
	for _, site := range catalogSites {
		for _, m := range catalogs[site] {
			if seen[m.ID] {
				continue
			}
			// Models disabled by config do not appear in any model list (/v1/models, monitor stats table, health counts).
			if disabled, _ := modelDisabled(m.ID); disabled && !includeDisabled {
				continue
			}
			seen[m.ID] = true
			ids = append(ids, m.ID)
		}
	}
	return ids, source
}

func modelEntry(site, modelID string) (catalogModel, bool) {
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	modelID = normalizeModelName(modelID)
	for _, m := range catalogModels[site] {
		if m.ID == modelID {
			return m, true
		}
	}
	return catalogModel{}, false
}

// modelCapabilitySite is the capability a single site declares for a model.
type modelCapabilitySite struct {
	Ceiling   int   `json:"context_length,omitempty"`         // Capability ceiling (maxInputTokens, falls back to maxAllowedSize when missing)
	MaxOutput int   `json:"max_output_tokens,omitempty"`      // Output ceiling
	Default   int   `json:"default_context_length,omitempty"` // Client default budget (not a capability ceiling)
	Options   []int `json:"context_length_options,omitempty"` // Client-selectable tiers
}

// modelCapability is the capability summary exposed by /v1/models.
// Top-level scalars are only filled when sites agree (or only one site has
// trusted data); when sites differ, the top level is left empty and BySite is
// filled, preventing downstream from reserving context for the larger value.
type modelCapability struct {
	Ceiling   int
	MaxOutput int
	Default   int
	Options   []int
	BySite    map[string]modelCapabilitySite
}

func sameIntSlice(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// capabilityScalar decides whether a scalar capability agrees across sites.
// Any site missing the value is treated as "not guaranteed" and returns false
// (unknown must not masquerade as agreement).
func capabilityScalar(bySite map[string]modelCapabilitySite, pick func(modelCapabilitySite) int) (int, bool) {
	value, found := 0, false
	for _, site := range catalogSites {
		v, ok := bySite[site]
		if !ok {
			continue
		}
		got := pick(v)
		if got <= 0 {
			return 0, false
		}
		if found && got != value {
			return 0, false
		}
		value, found = got, true
	}
	return value, found
}

func capabilityOptions(bySite map[string]modelCapabilitySite) ([]int, bool) {
	var first []int
	for _, site := range catalogSites {
		v, ok := bySite[site]
		if !ok {
			continue
		}
		if len(v.Options) == 0 {
			return nil, false
		}
		if first == nil {
			first = v.Options
			continue
		}
		if !sameIntSlice(first, v.Options) {
			return nil, false
		}
	}
	return first, len(first) >= 2
}

// modelCapabilityFor aggregates capability info for the model across site catalogs.
//
// The same model may be served by domestic and international accounts
// separately, and capabilities need not match. The maximum is not taken here:
// top-level scalars are only given when sites agree; otherwise they are left
// empty with per-site details in BySite, and the caller decides how to present
// them. A model with no trusted data returns false and the caller omits the field.
func modelCapabilityFor(modelID string) (modelCapability, bool) {
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	modelID = normalizeModelName(modelID)
	bySite := map[string]modelCapabilitySite{}
	for _, site := range catalogSites {
		for _, m := range catalogModels[site] {
			if m.ID != modelID {
				continue
			}
			ceiling := m.MaxInputTokens
			if ceiling <= 0 {
				ceiling = m.MaxAllowedSize
			}
			site0 := modelCapabilitySite{
				Ceiling: ceiling, MaxOutput: m.MaxOutputTokens,
				Default: m.ContextDefaultLength, Options: append([]int(nil), m.ContextLengths...),
			}
			if site0.Ceiling <= 0 && site0.MaxOutput <= 0 && site0.Default <= 0 && len(site0.Options) == 0 {
				break // this site has no trusted capability data; it does not participate in the summary
			}
			bySite[site] = site0
			break
		}
	}
	if len(bySite) == 0 {
		return modelCapability{}, false
	}
	cap := modelCapability{}
	cap.Ceiling, _ = capabilityScalar(bySite, func(v modelCapabilitySite) int { return v.Ceiling })
	cap.MaxOutput, _ = capabilityScalar(bySite, func(v modelCapabilitySite) int { return v.MaxOutput })
	cap.Default, _ = capabilityScalar(bySite, func(v modelCapabilitySite) int { return v.Default })
	if opts, ok := capabilityOptions(bySite); ok {
		cap.Options = opts
	}
	if len(bySite) > 1 {
		// When any top-level field failed to unify, give per-site details to avoid losing information.
		for _, site := range catalogSites {
			v, ok := bySite[site]
			if !ok {
				continue
			}
			if v.Ceiling != cap.Ceiling || v.MaxOutput != cap.MaxOutput ||
				v.Default != cap.Default || !sameIntSlice(v.Options, cap.Options) {
				cap.BySite = bySite
				break
			}
		}
	}
	return cap, true
}

func modelMultiplier(site, modelID string) (float64, bool) {
	entry, ok := modelEntry(site, modelID)
	if !ok {
		return 0, false
	}
	return entry.Multiplier, entry.HasMultiplier
}

func modelProbeVerdict(site, modelID string) string {
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	return modelProbes[probeKey(site, modelID)].Verdict
}

// modelDisplayMultiplier generates the multiplier text for the monitor side table.
// An expired promo only means the discount ended and the multiplier falls back
// to the credits base price; it is not the same as "price unknown".
// Only when a probe confirms paid and the API gives no valid multiplier is
// "Paid (unknown multiplier)" shown.
func modelDisplayMultiplier(site, modelID, freeLabel string) string {
	if freeLabel == "Yes" {
		return "0.00x"
	}
	switch modelProbeVerdict(site, modelID) {
	case "free":
		return "0.00x"
	case "paid":
		return "Paid (unknown multiplier)"
	}
	entry, ok := modelEntry(site, modelID)
	if !ok || !entry.HasMultiplier {
		return "-"
	}
	// When the promo has expired, the API's credits are untrusted (upstream often
	// bakes the promo price into credits); before a probe result exists, treat as
	// fully unknown and do not show a possibly stale price.
	if entry.PromoExpired {
		return "-"
	}
	return fmt.Sprintf("%.2fx", entry.Multiplier)
}

// siteKnownFree decides whether the model on a site is confirmed free (probe or explicit API 0.00x).
func siteKnownFree(site, modelID string) bool {
	if modelProbeVerdict(site, modelID) == "free" {
		return true
	}
	accountFree := func() (knownFree bool) {
		accountMu.Lock()
		skipped := 0
		defer func() {
			accountMu.Unlock()
			if skipped > 0 {
				log.Printf("[Site free check] site=%s model=%s skipped unschedulable accounts=%d valid account free record=%t note=empty accounts, disabled marks, and accounts missing credentials are excluded",
					site, modelID, skipped, knownFree)
			}
		}()
		for _, acc := range accounts {
			// At startup, .disabled markers are loaded as placeholder accounts without Auth;
			// they cannot participate in scheduling, nor influence site selection via historical free state.
			if acc == nil || acc.Disabled || acc.Auth == nil || acc.Auth.Auth.AccessToken == "" {
				skipped++
				continue
			}
			if acc.Profile().Key != site && acc.Edition != site {
				continue
			}
			if st := acc.ModelStates[normalizeModelName(modelID)]; st != nil && st.CostClass == modelCostFree {
				return true
			}
		}
		return false
	}()
	if accountFree {
		return true
	}
	entry, ok := modelEntry(site, modelID)
	return ok && entry.HasMultiplier && entry.Multiplier == 0 && !entry.PromoExpired
}

// siteKnownPaid decides whether the model on a site is confirmed paid.
func siteKnownPaid(site, modelID string) bool {
	if modelProbeVerdict(site, modelID) == "paid" {
		return true
	}
	entry, ok := modelEntry(site, modelID)
	return ok && entry.HasMultiplier && entry.Multiplier > 0
}

// preferredFreeSites returns the set of "free sites that should be preferred":
// priority only applies when "one site is free and another is paid"; when both
// are free or both are paid there is no priority.
func preferredFreeSites(modelID string) map[string]bool {
	var free, paid []string
	for _, site := range catalogSites {
		if siteKnownFree(site, modelID) {
			free = append(free, site)
		}
		if siteKnownPaid(site, modelID) {
			paid = append(paid, site)
		}
	}
	if len(free) == 0 || len(paid) == 0 {
		return nil
	}
	out := make(map[string]bool, len(free))
	for _, site := range free {
		out[site] = true
	}
	return out
}

func modelSourceLabel(source string) string {
	if source == "" {
		return "unavailable"
	}
	if strings.HasPrefix(source, "live-api@") {
		return source
	}
	return "live-api@" + source
}

// modelNeedsProbe decides whether a model on a site needs a price probe.
func modelNeedsProbe(site, modelID string, requests int64) bool {
	entry, ok := modelEntry(site, modelID)
	if !ok || !entry.FromLive {
		// Not in the live API catalog: probe only models actually requested, to avoid burning quota for nothing.
		return requests > 0
	}
	return entry.HasMultiplier && entry.Multiplier == 0 && entry.PromoExpired
}

// -----------------------------------------------------------------------------
// Sync
// -----------------------------------------------------------------------------

func refreshModelsOnce(schedule ...time.Time) {
	next := time.Time{}
	if len(schedule) > 0 {
		next = schedule[0]
	}
	trace, started := startScheduledTask("model_catalog", next)
	version, verErr := fetchNPMCatalogVersion()
	if verErr != nil {
		log.Printf("[Models] npm version query failed (only affects the npm fallback source): %v", verErr)
	}

	prev, prevSource := catalogSnapshot()
	fetched := map[string][]catalogModel{}
	var failures []string

	for _, site := range catalogSites {
		var live, npm []catalogModel
		liveOK, npmOK := false, false

		if models, promos, err := fetchLiveCatalog(site); err != nil {
			failures = append(failures, fmt.Sprintf("%s/API: %v", site, err))
			log.Printf("[Models] live catalog failed: site=%s, reason=%v", site, err)
		} else {
			live, liveOK = models, true
			log.Printf("[Models] live catalog succeeded: site=%s, model count=%d, active promos=%d", site, len(models), promos)
		}

		if version != "" {
			if models, transport, err := fetchNPMCatalog(version, npmCatalogFiles[site]); err != nil {
				failures = append(failures, fmt.Sprintf("%s/npm: %v", site, err))
				log.Printf("[Models] npm catalog failed: site=%s, reason=%v", site, err)
			} else {
				npm, npmOK = models, true
				log.Printf("[Models] npm catalog succeeded: site=%s, model count=%d, source=%s", site, len(models), transport)
			}
		}

		switch {
		case liveOK && npmOK:
			fetched[site] = mergeCatalogs(live, npm)
		case liveOK:
			fetched[site] = live
		case npmOK:
			fetched[site] = npm
		default:
			if cached, ok := prev[site]; ok && len(cached) > 0 {
				fetched[site] = cached
				log.Printf("[Models] site=%s both sources failed, keeping cache (model count=%d)", site, len(cached))
			} else {
				log.Printf("[Models] site=%s both sources failed and no cache; this site shows no models this round", site)
			}
		}
	}

	// Capability coverage: logged once per catalog refresh, not on each /v1/models
	// call, so humans can audit "which models have trusted context/output
	// capabilities and which sites are still missing".
	for _, site := range catalogSites {
		models, ok := fetched[site]
		if !ok {
			continue
		}
		ceiling, options, output := 0, 0, 0
		for _, m := range models {
			if m.MaxInputTokens > 0 || m.MaxAllowedSize > 0 {
				ceiling++
			}
			if len(m.ContextLengths) >= 2 {
				options++
			}
			if m.MaxOutputTokens > 0 {
				output++
			}
		}
		log.Printf("[Model capability] traceId=%s site=%s model count=%d with context ceiling=%d with selectable tiers=%d with output ceiling=%d note=records catalog coverage only; no upstream parameters injected and no output budget changed", trace, site, len(models), ceiling, options, output)
	}

	if len(fetched) == 0 {
		logScheduledTask("model_catalog", "completed", "failed", trace, started, next, "both catalogs unavailable, keeping prior state")
		log.Printf("[Models] no usable catalog data at all, keeping prior state; failure details: %s", strings.Join(failures, " | "))
		return
	}

	allCached := true
	for site := range fetched {
		if _, ok := prev[site]; !ok || len(prev[site]) == 0 {
			allCached = false
		}
	}

	source := time.Now().Format("2006-01-02 15:04")
	if allCached && prevSource != "" {
		source = prevSource
	}

	modelsMu.Lock()
	catalogModels = fetched
	dynamicSource = source
	// When the API reports 0.00x again and the promo is not expired (or there never
	// was one), clear the "paid" verdict and restore Free.
	for site, models := range fetched {
		for _, m := range models {
			if m.HasMultiplier && m.Multiplier == 0 && !m.PromoExpired {
				key := probeKey(site, m.ID)
				if p, ok := modelProbes[key]; ok && p.Verdict == "paid" {
					delete(modelProbes, key)
					log.Printf("[Models] site=%s model=%s API reported 0.00x again, clearing paid verdict", site, m.ID)
				}
			}
		}
	}
	cacheErr := persistModelsLocked(source)
	modelsMu.Unlock()
	refreshedAt := time.Now()
	for _, site := range catalogSites {
		if models, ok := fetched[site]; ok && len(models) > 0 {
			logOfficialRates(site, models, refreshedAt)
		}
	}
	requestModelsProbe()
	status := "success"
	if len(failures) > 0 || verErr != nil || cacheErr != nil {
		status = "partial"
	}
	logScheduledTask("model_catalog", "completed", status, trace, started, next, fmt.Sprintf("usable catalogs=%d, failed sources=%d, npm version query failed=%t, cache write failed=%t", len(fetched), len(failures), verErr != nil, cacheErr != nil))
}

func requestModelsScan() {
	select {
	case modelsScanTrigger <- struct{}{}:
	default:
	}
}

// requestModelsProbe triggers a round of price probing (called on catalog changes, credential changes, after reset).
func requestModelsProbe() {
	select {
	case modelProbeTrigger <- struct{}{}:
	default:
	}
}

func modelsRefreshLoop(interval time.Duration) {
	if interval <= 0 {
		return
	}
	refreshModelsOnce()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	next := time.Now().Add(interval)
	logScheduledTask("model_catalog", "scheduled", "", newTraceID(), time.Time{}, next, "fixed interval starts after the catalog refresh completes")
	for {
		select {
		case tick := <-ticker.C:
			next = tick.Add(interval)
			refreshModelsOnce(next)
		case <-modelsScanTrigger:
			log.Printf("[Models] account pool changed, refreshing model catalog immediately")
			refreshModelsOnce(next)
		}
	}
}

// modelPriceProbeLoop periodically runs real probes for models that "need price confirmation".
// modelPriceProbeLoop periodically runs price probes.
// The first round runs shortly after startup; while models remain pending it
// catches up with a shorter interval, returning to the long interval once converged.
func modelPriceProbeLoop() {
	trace := newTraceID()
	if !cfg.automaticPriceProbesAllowed() {
		reason := "disabled by default; -allow-price-probes=true not explicitly set"
		if cfg.DisablePriceProbes {
			reason = "legacy disable takes precedence: -disable-price-probes=true"
		}
		logScheduledTask("price_probe", "disabled", "disabled", trace, time.Time{}, time.Time{}, reason+"; no model requests are made automatically and no credits are spent on background price probes")
		log.Printf("[ModelPrice] traceId=%s stage=auto probe scheduling allowPriceProbes=%t legacyDisablePriceProbes=%t result=disabled reason=%s business impact=no model requests are made automatically; client requests, catalog refresh, and explicit probes are unaffected",
			trace, cfg.AllowPriceProbes, cfg.DisablePriceProbes, reason)
		return
	}
	log.Printf("[ModelPrice] traceId=%s stage=auto probe scheduling allowPriceProbes=%t legacyDisablePriceProbes=%t result=enabled reason=-allow-price-probes explicitly set cost warning=will issue real model generation requests that may consume credits; no cumulative credit budget limit",
		trace, cfg.AllowPriceProbes, cfg.DisablePriceProbes)
	timer := time.NewTimer(modelPriceProbeStartDelay)
	defer timer.Stop()
	logScheduledTask("price_probe", "scheduled", "", trace, time.Time{}, time.Now().Add(modelPriceProbeStartDelay), "explicitly enabled; real requests may consume credits; first round 20 seconds after startup; no requests generated when there are no candidate models")
	for {
		select {
		case <-timer.C:
		case <-modelProbeTrigger:
		}
		modelPriceProbeOnce()
		next := modelPriceProbeTick
		if modelPendingProbeCount() > 0 {
			next = modelPriceProbeCatchUp
		}
		timer.Reset(next)
		logScheduledTask("price_probe", "scheduled", "", newTraceID(), time.Time{}, time.Now().Add(next), "2 or 30 minutes depending on pending probes; same-site models limited to 12 hours")
	}
}

// modelPendingProbeCount counts the site+model pairs still needing a probe (past the probe interval).
func modelPendingProbeCount() int {
	ids, _ := mergedModelIDs()
	now := time.Now()
	pending := 0
	for _, site := range catalogSites {
		for _, id := range ids {
			if !modelNeedsProbe(site, id, modelRequestCount(id)) {
				continue
			}
			if pickProbeAccountForModel(site, id) == nil {
				continue
			}
			modelsMu.RLock()
			last := modelProbes[probeKey(site, id)].LastProbeAt
			modelsMu.RUnlock()
			if last > 0 && now.Sub(time.Unix(last, 0)) < modelAPIPriceProbeInterval {
				continue
			}
			pending++
		}
	}
	return pending
}

// modelProbeBatchSize returns the batch size for this round: enlarged on the
// first round (no probe records yet) so new installs / resets converge quickly;
// afterwards back to the regular batch.
func modelProbeBatchSize() int {
	modelsMu.RLock()
	n := len(modelProbes)
	modelsMu.RUnlock()
	if n == 0 {
		return modelPriceProbeInitialBatch
	}
	return modelPriceProbeBatch
}

func modelPriceProbeOnce() {
	trace, started := startScheduledTask("price_probe", time.Time{})
	ids, _ := mergedModelIDs()
	if len(ids) == 0 {
		logScheduledTask("price_probe", "completed", "skipped", trace, started, time.Time{}, "catalog is empty; no probe requests")
		return
	}
	now := time.Now()
	batch := modelProbeBatchSize()
	done := 0
	failed := 0
	for _, site := range catalogSites {
		if done >= batch {
			break
		}
		for _, id := range ids {
			if done >= batch {
				break
			}
			if !modelNeedsProbe(site, id, modelRequestCount(id)) {
				continue
			}
			acc := pickProbeAccountForModel(site, id)
			if acc == nil {
				continue
			}
			modelsMu.RLock()
			last := modelProbes[probeKey(site, id)].LastProbeAt
			modelsMu.RUnlock()
			if last > 0 && now.Sub(time.Unix(last, 0)) < modelAPIPriceProbeInterval {
				continue
			}
			verdict, credit, tokens, detail := probeModelPrice(acc, id)
			modelsMu.Lock()
			p := modelProbes[probeKey(site, id)]
			p.LastProbeAt = now.Unix()
			p.Credit, p.Tokens, p.Detail = credit, tokens, detail
			if verdict != "" {
				p.Verdict = verdict
			}
			modelProbes[probeKey(site, id)] = p
			cacheErr := persistModelsLocked(dynamicSource)
			modelsMu.Unlock()
			if verdict == "" || cacheErr != nil {
				failed++
			}
			log.Printf("[ModelPrice] site=%s account=%s model=%s -> verdict=%s credit=%s tokens=%d (%s)",
				site, acc.Path, id, p.Verdict, formatQuota(credit), tokens, detail)
			done++
		}
	}
	if done > 0 {
		writeStatusSnapshot()
	}
	status := "success"
	if done == 0 {
		status = "skipped"
	} else if failed == done {
		status = "failed"
	} else if failed > 0 {
		status = "partial"
	}
	logScheduledTask("price_probe", "completed", status, trace, started, time.Time{}, fmt.Sprintf("probes=%d, verdict or cache errors=%d", done, failed))
}

// pickProbeAccount selects a same-site account for price probing.
// Prefers accounts "known to have balance"; otherwise falls back to accounts
// with "unknown quota" (e.g. accounts whose quota API returns 20017 no
// permission) — a probe hitting 14018 is ignored and never pollutes the verdict.
// Only "known exhausted" and disabled accounts are skipped.
func pickProbeAccount(site string) *Account {
	return pickProbeAccountForModel(site, "")
}

func pickProbeAccountForModel(site, model string) *Account {
	accountMu.Lock()
	defer accountMu.Unlock()
	var unknownQuota *Account
	for _, acc := range accounts {
		if acc.Disabled || acc.Auth == nil || acc.Auth.Auth.AccessToken == "" {
			continue
		}
		if profileForEdition(acc.Auth.Edition).Key != site {
			continue
		}
		if allowed, reason := modelAccountAllowed(model, acc, accounts); !allowed {
			log.Printf("[ModelPrice] site=%s model=%s credential=%s result=skipped reason=account allowlist%s", site, model, filepath.Base(acc.Path), reason)
			continue
		}
		if acc.QuotaKnown {
			if acc.QuotaUnlimited || acc.QuotaRemaining > 0 {
				return acc
			}
			continue // known exhausted, skip
		}
		if unknownQuota == nil {
			unknownQuota = acc
		}
	}
	return unknownQuota
}

// probeModelPrice sends one minimal request to decide free/paid; it does not modify account scheduling state.
// Returns verdict (free|paid|""), credit, tokens, and note.
func probeModelPrice(acc *Account, model string) (string, float64, int64, string) {
	if !lockAccountWithContext(context.Background(), &acc.lock) {
		return "", 0, 0, "failed to acquire account lock"
	}
	defer acc.lock.Unlock()

	accountMu.Lock()
	if allowed, reason := modelAccountAllowed(model, acc, accounts); !allowed {
		accountMu.Unlock()
		log.Printf("[ModelPrice] traceId=%s model=%s credential=%s result=skipped reason=%s note=hot config re-checked after acquiring the account lock; upstream not called", newTraceID(), model, filepath.Base(acc.Path), reason)
		return "", 0, 0, "excluded by account allowlist: " + reason
	}
	if acc.Disabled || acc.Auth == nil {
		accountMu.Unlock()
		return "", 0, 0, "account unavailable"
	}
	auth := *acc.Auth
	prof := acc.Profile()
	accountMu.Unlock()
	if disabled, reason := modelDisabled(model); disabled {
		return "", 0, 0, reason
	}

	payload, _ := json.Marshal(map[string]any{
		"model": model, "stream": true, "max_tokens": 300,
		"messages": []any{
			map[string]any{"role": "system", "content": defaultSystemPrompt},
			map[string]any{"role": "user", "content": probePrompt},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, prof.chatURL(), strings.NewReader(string(payload)))
	if err != nil {
		return "", 0, 0, err.Error()
	}
	backendHeaders(req, &auth, prof)
	resp, err := cfg.HttpClient.Do(req)
	if err != nil {
		return "", 0, 0, "request failed: " + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		s := string(body)
		switch {
		case isQuotaExhausted(resp.StatusCode, s):
			return "", 0, 0, "14018 quota exhausted, not overriding"
		case isModelRateLimited(s):
			return "", 0, 0, "6004 model rate limited, not overriding"
		default:
			return "", 0, 0, fmt.Sprintf("HTTP %d, not overriding", resp.StatusCode)
		}
	}
	usage := readUsageFromSSE(resp.Body)
	credit, ok := usageCredit(usage)
	if !ok {
		return "", 0, 0, "no usage.credit, not overriding"
	}
	tokens, _ := usageTotalTokens(usage)
	if credit > 0 {
		return "paid", credit, tokens, "usage.credit>0 paid"
	}
	if tokens < modelFreeMinTokens {
		return "", credit, tokens, "credit=0 but sample too small, no verdict"
	}
	return "free", credit, tokens, "usage.credit=0 free"
}

// sortModelsForDisplay keeps a stable order: live API order first, then by ID.
func sortModelsForDisplay(models []catalogModel) []catalogModel {
	out := append([]catalogModel(nil), models...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FromLive != out[j].FromLive {
			return out[i].FromLive
		}
		return out[i].ID < out[j].ID
	})
	return out
}
