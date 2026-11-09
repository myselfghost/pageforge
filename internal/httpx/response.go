package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/pageforge/pageforge/internal/observability"
)

// MaxJSONBody bounds request bodies decoded by the control-plane API.
const MaxJSONBody = 1 << 20

// Error is an API error carrying the HTTP status and stable error code.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return e.Code + ": " + e.Message
}

// Errorf builds an *Error.
func Errorf(status int, code, format string, args ...any) error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// NotFound reports a missing object with the given error code.
func NotFound(code, message string) error {
	return &Error{Status: http.StatusNotFound, Code: code, Message: message}
}

// Conflict reports a uniqueness or state conflict.
func Conflict(code, message string) error {
	return &Error{Status: http.StatusConflict, Code: code, Message: message}
}

// Invalid reports a validation failure.
func Invalid(code, message string) error {
	return &Error{Status: http.StatusBadRequest, Code: code, Message: message}
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Default().Error("write json response", slog.String("error", err.Error()))
	}
}

// WriteError maps err to the documented error envelope.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		slog.Default().Error("unhandled api error",
			slog.String("request_id", observability.RequestIDFromContext(r.Context())),
			slog.String("error", err.Error()),
		)
		apiErr = &Error{Status: http.StatusInternalServerError, Code: "INTERNAL", Message: "internal error"}
	}
	var body errorEnvelope
	body.Error.Code = apiErr.Code
	body.Error.Message = apiErr.Message
	body.Error.RequestID = observability.RequestIDFromContext(r.Context())
	WriteJSON(w, apiErr.Status, body)
}

// DecodeJSON decodes a single JSON object body, rejecting unknown fields.
func DecodeJSON(r *http.Request, v any) error {
	defer func() { _, _ = io.Copy(io.Discard, r.Body) }()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, MaxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return Invalid("INVALID_JSON", "invalid JSON body: "+err.Error())
	}
	if dec.More() {
		return Invalid("INVALID_JSON", "request body must contain a single JSON object")
	}
	return nil
}
