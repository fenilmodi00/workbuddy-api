package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const planUnknown = "Unconfirmed"

// planEnterprise only means "this is enterprise-member allocated quota";
// it is not a personal plan code. Enterprise accounts have no personal
// resource packages, so a failed lookup must not be reported as free or trial.
const planEnterprise = "Enterprise"

// These codes are short names for legacy plans only, not request filters.
// New codes are still queried and the upstream PackageName is shown with
// priority, so newly added official plans do not silently fall back to "Free".
var legacyPlanNames = map[string]string{
	"TCACA_code_001_PqouKr6QWV": "Free",
	"TCACA_code_035_ArVxJcGDsm": "Free",
	"TCACA_code_008_cfWoLwvjU4": "Free",
	"TCACA_code_002_AkiJS3ZHF5": "pro",
	"TCACA_code_003_FAnt7lcmRT": "pro",
	"TCACA_code_039_KRcQj7wUat": "Pro trial",
	"TCACA_code_040_mi9rCYg46x": "Pro trial",
}

// The same Pro code is shown as "Standard" on the CN official site and must
// not reuse the international name. This table only normalizes the display of
// the four known tiers and is not used to filter query results; new codes
// still show their official names.
var cnPlanNames = map[string]string{
	"TCACA_code_008_cfWoLwvjU4": "Trial",
	"TCACA_code_002_AkiJS3ZHF5": "Standard",
	"TCACA_code_026_BaESVICNoi": "Premium",
	"TCACA_code_027_0FCGVA6vSa": "Flagship",
}

func knownPlanName(site, code string) string {
	if site == "cn" {
		return cnPlanNames[code]
	}
	return legacyPlanNames[code]
}

func freePlanName(site string) string {
	if site == "cn" {
		return "Trial"
	}
	return "Free"
}

var nonPlanCodes = map[string]bool{
	"TCACA_code_006_DbXS0lrypC": true,
	"TCACA_code_007_nzdH5h4Nl0": true,
	"TCACA_code_009_0XmEQc2xOf": true,
	"TCACA_code_036_lupO5WgNdG": true,
	"TCACA_code_037_WxOD3MpI2o": true,
}

type planResource struct {
	AccountID          json.RawMessage `json:"AccountId"` // official site may return a number or a string.
	ResourceID         string          `json:"ResourceId"`
	PackageCode        string          `json:"PackageCode"`
	PackageName        string          `json:"PackageName"`
	SubProductCode     string          `json:"SubProductCode"`
	Status             json.RawMessage `json:"Status"`
	DeductionStartTime json.RawMessage `json:"DeductionStartTime"`
	DeductionEndTime   json.RawMessage `json:"DeductionEndTime"`
	ExpiredTime        json.RawMessage `json:"ExpiredTime"`
	CycleEndTime       json.RawMessage `json:"CycleEndTime"`
}

func planInteger(raw json.RawMessage) (int64, error) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("numeric field missing or its format changed")
	}
	return n, nil
}

func planTimestamp(raw json.RawMessage) (time.Time, bool, error) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" || s == "0" {
		return time.Time{}, false, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n <= 0 {
			return time.Time{}, false, fmt.Errorf("invalid entitlement time")
		}
		if n > 1e12 {
			return time.UnixMilli(n), true, nil
		}
		return time.Unix(n, 0), true, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true, nil
	}
	// The user center has no timezone; timestamps and official-site display use UTC+8,
	// independent of the deployment machine's timezone.
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.FixedZone("UTC+8", 8*3600)); err == nil {
		return t, true, nil
	}
	return time.Time{}, false, fmt.Errorf("unrecognized entitlement time format")
}

func activePlanResource(r planResource, now time.Time) (bool, error) {
	status, err := planInteger(r.Status)
	if err != nil {
		return false, err
	}
	switch status {
	case 1, 2: // refunded, expired; points exhausted (3) is not plan expiry.
		return false, nil
	case 0, 3:
	default:
		return false, fmt.Errorf("unknown entitlement status %d", status)
	}
	start, present, err := planTimestamp(r.DeductionStartTime)
	if err != nil {
		return false, err
	}
	if present && now.Before(start) {
		return false, nil
	}
	// Same entitlement deadline field priority as the official site; the monthly
	// quota cycle is not the annual plan's expiry.
	for _, raw := range []json.RawMessage{r.DeductionEndTime, r.ExpiredTime, r.CycleEndTime} {
		end, present, err := planTimestamp(raw)
		if err != nil {
			return false, err
		}
		if present {
			return now.Before(end), nil
		}
	}
	return true, nil // Status valid and no deadline: long-term entitlement.
}

func safePlanName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1 // Upstream names must not inject terminal escapes, newlines, or directional controls.
		}
		return r
	}, name)
	runes := []rune(strings.TrimSpace(name))
	if len(runes) > 80 {
		runes = runes[:80]
	}
	return string(runes)
}

func resourcePlanName(site string, r planResource) string {
	if site == "cn" {
		if label := cnPlanNames[r.PackageCode]; label != "" {
			return label
		}
		label := safePlanName(r.PackageName)
		for _, tier := range []string{"Trial", "Standard", "Premium", "Flagship"} {
			switch label {
			case tier, "Personal " + tier, "CodeBuddy Personal " + tier:
				return tier
			}
		}
		if label != "" {
			return label // New plans and historical junior tiers are not forced into the current four tiers.
		}
		return "Unrecognized subscription"
	}
	if label := safePlanName(r.PackageName); label != "" {
		switch strings.ToLower(label) {
		case "free plan subscription", "free plan":
			return "Free"
		case "pro plan monthly subscription", "pro plan annual subscription":
			return "pro"
		case "pro plan trial subscription":
			return "Pro trial"
		default:
			return label
		}
	}
	if label := legacyPlanNames[r.PackageCode]; label != "" {
		return label
	}
	return "Unrecognized subscription"
}

func identifyPlan(site string, summary quotaSummaryData, resources []planResource, now time.Time) (string, error) {
	var plans []planResource
	ambiguous := false
	for _, r := range resources {
		if nonPlanCodes[r.PackageCode] || r.SubProductCode == "sp_tcaca_codebuddyide_bonus_pack" ||
			r.SubProductCode == "sp_tcaca_codebuddyide_creditplan" {
			continue
		}
		active, err := activePlanResource(r, now)
		if err != nil {
			return "", err
		}
		if !active {
			continue
		}
		if r.PackageCode == "" {
			return "", fmt.Errorf("active entitlement missing plan code")
		}
		// New plans reusing the IDE sub-product, or the current subscription
		// explicitly named in summary, are both recognizable.
		if r.SubProductCode == "sp_tcaca_codebuddy_ide" || knownPlanName(site, r.PackageCode) != "" ||
			r.PackageCode == summary.SubscriptionPackageCode {
			plans = append(plans, r)
		} else {
			ambiguous = true // New sub-products must not be silently ignored and then claimed as free.
		}
	}
	// When the official trial has converted to paid (2) and the trial is still
	// active, the trial is still shown; the paid plan awaits activation.
	trialStatus, _ := planInteger(json.RawMessage(fmt.Sprint(summary.ProTrialStatus)))
	if site == "intl" && trialStatus == 2 {
		for _, r := range plans {
			if legacyPlanNames[r.PackageCode] == "Pro trial" {
				return "Pro trial", nil
			}
		}
	}
	if summary.SubscriptionPackageCode != "" {
		for _, r := range plans {
			if r.PackageCode == summary.SubscriptionPackageCode {
				return resourcePlanName(site, r), nil
			}
		}
		return "", fmt.Errorf("subscription summary and active entitlements disagree")
	}
	// Without a summary-named plan, keep all valid identity package names;
	// never guess the tier from an unknown plan's name.
	labels := map[string]bool{}
	hasFree := false
	for _, r := range plans {
		label := resourcePlanName(site, r)
		if label == freePlanName(site) {
			hasFree = true
		} else {
			labels[label] = true
		}
	}
	if ambiguous {
		return "", fmt.Errorf("unknown sub-product found; plan cannot be fully confirmed")
	}
	if len(labels) > 0 {
		if site == "intl" && len(labels) > 1 {
			delete(labels, "Pro trial") // With an active real subscription, an ordinary trial does not override it.
		}
		names := make([]string, 0, len(labels))
		for name := range labels {
			names = append(names, name)
		}
		sort.Strings(names)
		return strings.Join(names, " / "), nil
	}
	if summary.IsPaidUser {
		return "", fmt.Errorf("paid flag present but no active subscription entitlement")
	}
	if hasFree || len(plans) == 0 {
		// Complete query succeeded: only expired/bonus packages or an empty list,
		// and no paid-subscription claim.
		return freePlanName(site), nil
	}
	return "", fmt.Errorf("plan could not be confirmed")
}

func parsePlanPage(data []byte) ([]planResource, int64, error) {
	var envelope struct {
		Response *struct {
			Data json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Response == nil {
		return nil, 0, fmt.Errorf("plan API response missing Response")
	}
	var page struct {
		Accounts   json.RawMessage
		TotalCount json.RawMessage
	}
	if err := json.Unmarshal(envelope.Response.Data, &page); err != nil {
		return nil, 0, fmt.Errorf("plan API Data format invalid")
	}
	total, err := planInteger(page.TotalCount)
	if err != nil || total < 0 {
		return nil, 0, fmt.Errorf("plan API TotalCount missing or invalid")
	}
	if len(bytes.TrimSpace(page.Accounts)) == 0 || bytes.TrimSpace(page.Accounts)[0] != '[' {
		return nil, 0, fmt.Errorf("plan API Accounts is not an array")
	}
	var rows []planResource
	if err := json.Unmarshal(page.Accounts, &rows); err != nil {
		return nil, 0, fmt.Errorf("plan API Accounts format invalid")
	}
	return rows, total, nil
}

// fetchPlanResources queries complete resources without filtering PackageCodes
// and never performs purchase/renewal/model requests.
func fetchPlanResources(ctx context.Context, prof *upstreamProfile, headers func(*http.Request), traceID, account string, now time.Time) ([]planResource, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var resources []planResource
	lastTotal := int64(-1)
	seen := map[string]bool{}
	local := now.In(time.FixedZone("UTC+8", 8*3600))
	for page := 1; page <= 10; page++ {
		payload, _ := json.Marshal(map[string]any{
			"PageNumber": page, "PageSize": 200, "ProductCode": "p_tcaca",
			"Status": []int{0, 3}, "OnlyValidPeriod": true,
			"SlicePeriodStartTime": local.Format("2006-01-02") + " 00:00:00",
			"SlicePeriodEndTime":   local.Format("2006-01-02") + " 23:59:59",
		})
		log.Printf("[Plan query] traceId=%s account=%s stage=read-only resource request page=%d result=started note=no plan-code filter; no model requests", traceID, account, page)
		data, status, err := doJSONContext(ctx, cfg.HttpClient, http.MethodPost,
			prof.Origin+"/billing/meter/get-user-resource", headers, bytes.NewReader(payload))
		if err != nil {
			// Do not log the upstream error body: it may contain user identifiers or tokens.
			log.Printf("[Plan query] traceId=%s account=%s page=%d HTTP=%d result=failed business impact=plan cannot be confirmed this round", traceID, account, page, status)
			return nil, fmt.Errorf("plan read-only API failed HTTP=%d", status)
		}
		rows, total, err := parsePlanPage(data)
		if err != nil {
			return nil, err
		}
		if lastTotal >= 0 && total != lastTotal {
			return nil, fmt.Errorf("plan total changed during query; waiting for the next round")
		}
		lastTotal = total
		for _, row := range rows {
			// One subscription resource may have multiple metering accounts;
			// deduplicate by AccountId first.
			key := strings.Trim(strings.TrimSpace(string(row.AccountID)), `"`)
			if key == "" || key == "null" {
				key = ""
				if row.ResourceID != "" {
					key = "resource:" + row.ResourceID
				}
			} else {
				key = "account:" + key
			}
			if key != "" {
				if seen[key] {
					return nil, fmt.Errorf("duplicate plan pages; refusing to use incomplete results")
				}
				seen[key] = true
			}
		}
		resources = append(resources, rows...)
		log.Printf("[Plan query] traceId=%s account=%s page=%d HTTP=%d this page=%d cumulative=%d total=%d result=parsed", traceID, account, page, status, len(rows), len(resources), total)
		if int64(len(resources)) == total {
			return resources, nil
		}
		if len(rows) == 0 || int64(len(resources)) > total {
			return nil, fmt.Errorf("plan pagination incomplete")
		}
	}
	return nil, fmt.Errorf("plan pagination exceeded the safety limit; partial results not used")
}

func planDisplayLabel(label string, stale bool) string {
	if label == "" {
		return planUnknown
	}
	if stale {
		return label + "*"
	}
	return label
}

func markAccountPlanStale(acc *Account) {
	accountMu.Lock()
	defer accountMu.Unlock()
	if acc.PlanCheckedAt == 0 {
		acc.PlanLabel = planUnknown
	}
	acc.PlanStale = true
}
