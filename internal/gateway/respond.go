package gateway

import (
	"encoding/json"
	"net/http"
)

// beginRequest stamps the response with a correlation id so a caller can quote
// one value when reporting a failure, and returns it for the error body.
func beginRequest(w http.ResponseWriter) string {
	requestID := newRequestID()
	if requestID != "" {
		w.Header().Set("X-Request-Id", requestID)
	}
	return requestID
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": "free_router_error"}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
