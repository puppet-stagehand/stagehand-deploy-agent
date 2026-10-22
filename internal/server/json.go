package server

import (
	"encoding/json"
	"net/http"
)

// writeJSON writes v as the response body with json.Marshal and one Write: no
// trailing newline and HTML escaping on, exactly the bytes gin's c.JSON wrote
// for the fork, so the Code Manager pack 0.1.0 sees an unchanged contract
// (RESEARCH Pattern 1). Never use a streaming json.Encoder for a contract
// response: it appends a newline.
//
// Error bodies are map[string]string, whose keys encoding/json sorts, so
// "error" always precedes "field" and "id".
func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}
