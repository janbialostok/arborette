package service

import (
	"encoding/json"
	"log"
	"net/http"
)

// errorResponse is the one error envelope every service answers with, so a
// client parses `{"error": ...}` the same way whichever service it reached.
type errorResponse struct {
	Error string `json:"error"`
}

// WriteJSON writes v as the JSON body of a status response.
//
// An encode failure is logged rather than returned: the status line is already
// on the wire by then, so there is no second response to send.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("service: encode response: %v", err)
	}
}

// WriteErr answers with the error envelope under status.
func WriteErr(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, errorResponse{Error: msg})
}
