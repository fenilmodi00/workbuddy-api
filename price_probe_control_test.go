package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAutomaticPriceProbeFlagsRequireExplicitOptIn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		allowed bool
	}{
		{name: "new installation defaults off"},
		{name: "explicit allow", args: []string{"-allow-price-probes"}, allowed: true},
		{name: "explicit true", args: []string{"-allow-price-probes=true"}, allowed: true},
		{name: "explicit false", args: []string{"-allow-price-probes=false"}},
		{name: "legacy disable", args: []string{"-disable-price-probes"}},
		{name: "legacy false is not consent", args: []string{"-disable-price-probes=false"}},
		{name: "disable wins after allow", args: []string{"-allow-price-probes", "-disable-price-probes"}},
		{name: "disable wins before allow", args: []string{"-disable-price-probes", "-allow-price-probes"}},
		{name: "legacy false with explicit consent", args: []string{"-disable-price-probes=false", "-allow-price-probes"}, allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Registering the flags must reset old values so a previous
			// version's / prior parse's enabled state is never carried over.
			target := Config{AllowPriceProbes: true, DisablePriceProbes: true}
			fs := flag.NewFlagSet("serve", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			registerAutomaticPriceProbeFlags(fs, &target)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			if got := target.automaticPriceProbesAllowed(); got != tc.allowed {
				t.Fatalf("args=%v allowed=%t want=%t", tc.args, got, tc.allowed)
			}
			oldCfg := cfg
			cfg = target
			t.Cleanup(func() { cfg = oldCfg })
			row := taskByID(t, taskViewDefinitions(), "price_probe")
			if row.Enabled != tc.allowed {
				t.Fatalf("task page enabled state disagrees with actual scheduling: %+v", row)
			}
			if !tc.allowed {
				now := time.Now()
				row = taskByID(t, aggregateTaskViews(nil, now, os.Getpid(), now.UnixMilli()), "price_probe")
				if row.Status != "disabled" || row.NextRunAt != 0 || row.Success != nil {
					t.Fatalf("unauthorized probe must not show a schedule or success: %+v", row)
				}
			}
		})
	}
	for _, args := range [][]string{
		{"-allow-price-probes=invalid"},
		{"-disable-price-probes=invalid"},
	} {
		target := Config{}
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		registerAutomaticPriceProbeFlags(fs, &target)
		if err := fs.Parse(args); err == nil {
			t.Fatalf("invalid boolean argument was accepted: %v", args)
		}
	}
}

func TestDisabledAutomaticPriceProbeStopsScheduler(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		config       Config
	}{
		{name: "zero config defaults off", reason: "disabled by default"},
		{name: "legacy disable overrides opt-in", reason: "legacy disable takes precedence", config: Config{AllowPriceProbes: true, DisablePriceProbes: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertDisabledPriceProbeScheduler(t, tc.config, tc.reason)
		})
	}
}

func assertDisabledPriceProbeScheduler(t *testing.T, config Config, reason string) {
	t.Helper()
	chdirTemp(t)
	oldCfg, oldWriter := cfg, log.Writer()
	cfg = config
	var audit bytes.Buffer
	closeLog := initFileLogging("test-price-probes")
	log.SetOutput(io.MultiWriter(log.Writer(), &audit))
	t.Cleanup(func() {
		cfg = oldCfg
		log.SetOutput(oldWriter)
		closeLog()
	})
	done := make(chan struct{})
	go func() {
		modelPriceProbeLoop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled price probe scheduler did not exit before starting any model requests")
	}
	for _, marker := range []string{"probe scheduling", "result=disabled", "no model requests are made automatically", reason} {
		if !strings.Contains(audit.String(), marker) {
			t.Errorf("missing probe shutdown audit marker %q", marker)
		}
	}
	// The disabled decision and the task-page event must use the same TraceID
	// and actually land in the project's logs directory.
	var event scheduledTaskEvent
	for _, line := range strings.Split(audit.String(), "\n") {
		if _, raw, ok := strings.Cut(line, "[Scheduled task] "); ok {
			if err := json.Unmarshal([]byte(raw), &event); err != nil {
				t.Fatal(err)
			}
		}
	}
	if event.Task != "price_probe" || event.Phase != "disabled" || event.Trace == "" || event.NextAt != 0 {
		t.Fatalf("missing disabled task event: %+v", event)
	}
	if !strings.Contains(audit.String(), "traceId="+event.Trace) {
		t.Fatal("disabled decision did not use the task log's TraceID")
	}
	content, err := os.ReadFile(filepath.Join(logDir, "gateway-"+time.Now().Format("2006-01-02")+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(content, []byte("traceId="+event.Trace)) {
		t.Fatal("disabled decision went only to the console; it was not written to the log file")
	}
}

func TestDefaultDisabledPriceProbeStillLearnsFromNormalRequests(t *testing.T) {
	isolatePolicies(t)
	cfg.AllowPriceProbes, cfg.DisablePriceProbes = false, false
	if cfg.automaticPriceProbesAllowed() {
		t.Fatal("active probing must not be allowed by default")
	}
	oldWriter := log.Writer()
	closeLog := initFileLogging("test-normal-price-learning")
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		closeLog()
	})
	acc := &Account{Path: "synthetic-normal-request.json", Auth: &StoredAuth{}}
	for _, tc := range []struct {
		model string
		usage map[string]any
		want  string
	}{
		{"learn-free", map[string]any{"credit": 0, "total_tokens": 100}, modelCostFree},
		{"learn-paid", map[string]any{"credit": 1.5, "total_tokens": 10}, modelCostPaid},
	} {
		observeModelCredit(acc, tc.model, tc.usage, 1)
		accountMu.Lock()
		state := acc.ModelStates[tc.model]
		accountMu.Unlock()
		if state == nil || state.CostClass != tc.want {
			t.Fatalf("disabling active probing affected normal request learning: model=%s state=%+v", tc.model, state)
		}
	}
}
