package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log"
	"net/http"
)

// The caller holds runtimeConfigMu; validate first, persist, then apply; on failure restore the old config.
func persistRuntimeDocument(doc map[string]json.RawMessage, old []byte, trace string) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	c, err := parseRuntimeConfig(data)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := writeConfigPreservingFile(data, old); err != nil {
		return err
	}
	if err := applyRuntimeConfig(c, true, trace); err != nil {
		if restoreErr := writeConfigPreservingFile(old, data); restoreErr != nil {
			return errors.New("config apply failed and the old config could not be restored; check the server logs")
		}
		return err
	}
	runtimeAppliedHash, runtimeHasAppliedHash = sha256.Sum256(data), true
	return nil
}

func writeAdminOperationError(w http.ResponseWriter, r *http.Request, operation string, err error) {
	status := http.StatusInternalServerError
	var validation gatewayValidationError
	if errors.As(err, &validation) {
		status = http.StatusBadRequest
	}
	if errors.Is(err, errConfigChange) || errors.Is(err, errCredentialChanged) {
		status = http.StatusConflict
	}
	log.Printf("[Admin operation failed] traceId=%s operation=%s status-code=%d reason=%s note=modification not completed; the page should keep user input for correction or retry", debugTraceID(r), operation, status, safeDebugError(err))
	writeJSONError(w, status, safeDebugError(err))
}
