package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// -----------------------------------------------------------------------------
// reset command
//
// Clears all local data except login credentials (workbuddy*.json):
//   - status snapshot workbuddy-status.json
//   - model cache wb-models-cache.json (including multipliers and price probe findings)
//   - disabled markers *.disabled / *.json.disabled
//   - run logs logs/
//
// Then re-fetches the model catalog and multipliers and writes a new cache.
// The in-memory account ledger is held by the serve process and cannot be
// modified directly by reset; while serve is running, restart the service so
// the in-memory state is zeroed together with the disk.
// -----------------------------------------------------------------------------

func runReset() {
	fmt.Println("================ WorkBuddy local data cleanup ================")

	authPaths := collectConfiguredAuthPaths()
	removed := 0

	removeIfExists := func(path, label string) {
		if err := os.Remove(path); err == nil {
			fmt.Printf("  deleted %s: %s\n", label, path)
			removed++
		} else if !os.IsNotExist(err) {
			fmt.Printf("  delete failed: %s: %v\n", path, err)
		}
	}

	// 1. Status snapshot and model cache (including temp files)
	for _, f := range []string{
		statusSnapshotFile, statusSnapshotFile + ".tmp",
		modelsCacheFile, modelsCacheFile + ".tmp",
	} {
		removeIfExists(f, "")
	}

	// 2. Disabled markers: marker files for explicitly configured credential paths
	for _, p := range authPaths {
		removeIfExists(markerPath(p), "disabled marker")
	}
	// Other marker files possibly left over in auto-discovery mode
	if entries, err := os.ReadDir("."); err == nil {
		for _, e := range entries {
			name := e.Name()
			if strings.HasSuffix(name, ".disabled") || strings.HasSuffix(name, ".json.disabled") {
				removeIfExists(name, "disabled marker")
			}
		}
	}

	// 3. Run logs
	if entries, err := os.ReadDir(logDir); err == nil {
		n := 0
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if err := os.Remove(filepath.Join(logDir, e.Name())); err == nil {
				n++
			}
		}
		if err := os.RemoveAll(logDir); err == nil {
			_ = os.MkdirAll(logDir, 0755)
		}
		fmt.Printf("  logs cleared: %s/ (%d files)\n", logDir, n)
		removed += n
	}

	fmt.Printf("cleanup complete; %d items deleted; login credentials preserved (%d)\n", removed, len(authPaths))

	// 4. Re-fetch the model catalog and multipliers
	fmt.Println("\nre-fetching the model catalog and multipliers...")
	loadModelsCache()
	if err := loadAccounts(); err != nil {
		fmt.Printf("no usable credentials found; skipping model fetch: %v\n", err)
	} else {
		refreshModelsOnce()
	}
	ids, source := mergedModelIDs()
	fmt.Printf("model catalog source: %s, model count: %d\n", modelSourceLabel(source), len(ids))
	fmt.Println("Note: if serve is running, restart the service so the in-memory state is zeroed too:")
	fmt.Println("  systemctl restart workbuddy-gateway")
	fmt.Println("=======================================================")
}
