// Package httpx holds shared HTTP transport concerns: the JSON response
// envelope, request-id propagation, panic recovery, security headers and an
// in-process token-bucket rate limiter.
package httpx

import (
	"encoding/json"
	"net/http"
)

// Envelope is the uniform API response body. Success responses use {data, meta};
// error responses use {error} — clients branch on the presence of "error".
type Envelope struct {
	Data  any    `json:"data,omitempty"`
	Meta  any    `json:"meta,omitempty"`
	Error *EInfo `json:"error,omitempty"`
}

// EInfo is the machine-readable error surface.
type EInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// WriteJSON writes a success envelope with the given status.
func WriteJSON(w http.ResponseWriter, status int, data, meta any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Envelope{Data: data, Meta: meta})
}

// WriteError writes the canonical error envelope. Application errors map
// through their kind; anything else is a 500 with an opaque message.
func WriteError(w http.ResponseWriter, err error) {
	appErr := appErrorOf(err)
	body := Envelope{Error: &EInfo{Code: appErr.Code, Message: appErr.Message, Details: appErr.Details}}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(appErr.HTTPStatus())
	_ = json.NewEncoder(w).Encode(body)
}
