package server

import (
	"encoding/json"
	"net/http"
)

type apiError struct {
	Error string `json:"error"`
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(apiError{Error: msg})
}
