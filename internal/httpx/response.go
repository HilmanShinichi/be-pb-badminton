package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
)

type Envelope struct {
	Data any `json:"data,omitempty"`
	Meta any `json:"meta,omitempty"`
}
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details"`
}

func OK(w http.ResponseWriter, status int, data any) {
	writeJSON(w, status, Envelope{Data: data})
}

func OKWithMeta(w http.ResponseWriter, status int, data, meta any) {
	writeJSON(w, status, Envelope{Data: data, Meta: meta})
}

func Err(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": ErrorBody{Code: code, Message: message}})
}

func WriteAppError(w http.ResponseWriter, err error) {
	ae := AsAppError(err)
	Err(w, ae.Status, ae.Code, ae.Message)
}

func Decode(r *http.Request, into any) error {
	return json.NewDecoder(r.Body).Decode(into)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// AppError carries a stable error code from service layer to HTTP status.
type AppError struct {
	Status  int
	Code    string
	Message string
}

func (e *AppError) Error() string { return e.Message }

func AsAppError(err error) *AppError {
	var ae *AppError
	if errors.As(err, &ae) {
		return ae
	}
	return &AppError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "Something went wrong on the server."}
}

func BadRequest(code, msg string) error { return &AppError{http.StatusBadRequest, code, msg} }
func Unauthorized(msg string) error     { return &AppError{http.StatusUnauthorized, "UNAUTHORIZED", msg} }
func NotFound(msg string) error         { return &AppError{http.StatusNotFound, "NOT_FOUND", msg} }
func Conflict(code, msg string) error   { return &AppError{http.StatusConflict, code, msg} }
func Unprocessable(msg string) error {
	return &AppError{http.StatusUnprocessableEntity, "VALIDATION_ERROR", msg}
}
