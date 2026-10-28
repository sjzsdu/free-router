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
	writeErrorCode(w, status, message, "")
}

// writeErrorCode writes the OpenAI error envelope every client SDK knows how to
// parse. The optional code is additive: without it a plain-text body (for
// example from http.Error) makes clients report a JSON decoding failure instead
// of the real reason.
func writeErrorCode(w http.ResponseWriter, status int, message, code string) {
	body := map[string]any{"message": message, "type": "free_router_error"}
	if code != "" {
		body["code"] = code
	}
	writeJSON(w, status, map[string]any{"error": body})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
