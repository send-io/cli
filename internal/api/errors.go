package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Exit codes: the CLI's contract with scripts and agents.
const (
	ExitOK          = 0
	ExitAPI         = 1
	ExitUsage       = 2
	ExitAuth        = 3
	ExitNetwork     = 4
	ExitNotFound    = 5
	ExitRateLimited = 6
	ExitTimeout     = 7
)

// Error is any failure the CLI reports: an API error response, a network
// failure, a usage mistake or a timeout. Exit is the process exit code.
type Error struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Status     int    `json:"status,omitempty"`
	Exit       int    `json:"-"`
	RetryAfter string `json:"retry_after,omitempty"`
	Hint       string `json:"hint,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Errorf builds a CLI error with an exit code.
func Errorf(exit int, code, format string, args ...any) *Error {
	return &Error{Exit: exit, Code: code, Message: fmt.Sprintf(format, args...)}
}

// UsageError is a mistake in how the command was invoked (exit 2).
func UsageError(format string, args ...any) *Error {
	return Errorf(ExitUsage, "usage_error", format, args...)
}

// ExitFor maps an HTTP status to the exit code.
func ExitFor(status int) int {
	switch {
	case status == 401 || status == 403:
		return ExitAuth
	case status == 404:
		return ExitNotFound
	case status == 429:
		return ExitRateLimited
	default:
		return ExitAPI
	}
}

// FromResponse decodes an API error body: the API's {statusCode, name,
// message}, OAuth's {error, error_description}, or anything else.
func FromResponse(status int, h http.Header, body []byte) *Error {
	e := &Error{Status: status, Exit: ExitFor(status)}
	if h != nil {
		e.RetryAfter = h.Get("Retry-After")
	}
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		if s, ok := m["name"].(string); ok {
			e.Code = s
		}
		if s, ok := m["message"].(string); ok {
			e.Message = s
		}
		if s, ok := m["error"].(string); ok && e.Code == "" {
			e.Code = s
		}
		if s, ok := m["error_description"].(string); ok && e.Message == "" {
			e.Message = s
		}
		if em, ok := m["error"].(map[string]any); ok {
			if s, ok := em["code"].(string); ok {
				e.Code = s
			}
			if s, ok := em["message"].(string); ok {
				e.Message = s
			}
		}
	}
	if e.Code == "" {
		e.Code = strings.ReplaceAll(strings.ToLower(http.StatusText(status)), " ", "_")
		if e.Code == "" {
			e.Code = "http_error"
		}
	}
	if e.Message == "" {
		txt := strings.TrimSpace(string(body))
		if len(txt) > 300 || strings.HasPrefix(txt, "<") {
			txt = ""
		}
		if txt == "" {
			txt = fmt.Sprintf("HTTP %d %s", status, http.StatusText(status))
		}
		e.Message = txt
	}
	switch e.Exit {
	case ExitAuth:
		if status == 401 {
			e.Hint = "Run `sendio login`, or pass --api-key / SENDIO_API_KEY."
		}
	case ExitRateLimited:
		if e.RetryAfter != "" {
			e.Hint = "Retry after " + e.RetryAfter + "s."
		}
	}
	return e
}

// AsError unwraps err to an *Error, wrapping unknown errors as API errors.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Exit: ExitAPI, Code: "error", Message: err.Error()}
}

// IsStatus reports whether err is an API error with the given status.
func IsStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}
