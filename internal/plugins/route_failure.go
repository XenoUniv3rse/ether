package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/logredact"
)

// ErrMalformedResponse marks a response the host could not decode or accept.
var ErrMalformedResponse = errors.New("malformed plugin response")

// RouteFailure is a redacted, bounded diagnostic for the owner console.
type RouteFailure struct {
	Code              string `json:"code"`
	Message           string `json:"message,omitempty"`
	Body              string `json:"body,omitempty"`
	Route             string `json:"route,omitempty"`
	Status            int    `json:"status,omitempty"`
	TimeoutSeconds    int    `json:"timeoutSeconds,omitempty"`
	RetryAfterSeconds int    `json:"retryAfterSeconds,omitempty"`
}

func (f *RouteFailure) Error() string {
	if f.Message != "" {
		return f.Message
	}
	return fmt.Sprintf("Plugin call failed (%s)", f.Code)
}

// Scrub whole URLs in error bodies: credentials can occur in arbitrary path
// segments and query keys that the normal diagnostic redactor cannot recognize.
var diagnosticCredential = regexp.MustCompile(`(?i)\b(api[-_ ]?key|access[-_ ]?token|password|passkey|client[-_ ]?secret)\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s,;]+)`)

var diagnosticURL = regexp.MustCompile("(?i)(?:[a-z][a-z0-9+.-]*(?:://|%3a%2f%2f)|//)[^\\s<>\"'\\\\]+")

const diagnosticBodyLimit = 64 << 10

// DiagnoseRouteResponse interprets a non-success response. redact removes
// plugin-specific credentials in addition to the standard diagnostic rules.
func DiagnoseRouteResponse(path string, resp Response, now time.Time, redact func(string) string) *RouteFailure {
	if resp.Status == 0 || resp.Status >= 200 && resp.Status < 300 {
		return nil
	}
	f := &RouteFailure{Code: "rejected", Route: cleanRoutePath(path, redact), Status: resp.Status}
	var env struct {
		Error             json.RawMessage `json:"error"`
		Message           string          `json:"message"`
		RetryAfterSeconds int             `json:"retryAfterSeconds"`
		RetryAfterSnake   int             `json:"retry_after_seconds"`
	}
	var detail struct {
		Code              string `json:"code"`
		Message           string `json:"message"`
		RetryAfterSeconds int    `json:"retryAfterSeconds"`
	}
	if len(resp.Body) <= diagnosticBodyLimit {
		_ = json.Unmarshal(resp.Body, &env)
		_ = json.Unmarshal(env.Error, &detail)
		body := strings.TrimSpace(string(resp.Body))
		if json.Valid(resp.Body) {
			body = redactJSON(resp.Body)
			// A JSON string is itself a plain-text diagnostic.
			if json.Unmarshal(resp.Body, &f.Message) != nil {
				f.Message = ""
			}
		} else if utf8.ValidString(body) && !strings.ContainsRune(body, 0) {
			if !strings.HasPrefix(body, "<") {
				f.Message = body
			}
		} else {
			body = ""
		}
		f.Body = truncate(cleanRouteDiagnostic(body, redact), 2048)
		message := detail.Message
		if message == "" {
			message = env.Message
		}
		if message == "" {
			message = f.Message
		}
		if message == "" {
			_ = json.Unmarshal(env.Error, &message)
		}
		f.Message = truncate(strings.Join(strings.Fields(cleanRouteDiagnostic(message, redact)), " "), 300)
	}
	switch detail.Code {
	case "auth", "rate_limited", "upstream_unavailable", "not_found", "invalid_settings":
		f.Code = detail.Code
	default:
		switch resp.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			f.Code = "auth"
		case http.StatusTooManyRequests:
			f.Code = "rate_limited"
		case http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			f.Code = "upstream_unavailable"
		case http.StatusNotFound:
			f.Code = "not_found"
		}
		if detail.Code == "internal_error" && resp.Status == http.StatusInternalServerError {
			f.Code = "crashed"
		}
	}
	if strings.EqualFold(f.Message, http.StatusText(resp.Status)) {
		f.Message = ""
	}
	for k, v := range resp.Headers {
		if !strings.EqualFold(k, "Retry-After") {
			continue
		}
		v = strings.TrimSpace(v)
		if seconds, err := strconv.Atoi(v); err == nil && seconds > 0 {
			f.RetryAfterSeconds = seconds
		} else if at, err := http.ParseTime(v); err == nil && at.After(now) {
			wait := at.Sub(now)
			seconds := int64(wait / time.Second)
			if wait%time.Second != 0 {
				seconds++
			}
			f.RetryAfterSeconds = int(min(seconds, int64(maxRouteRetrySeconds())))
		}
	}
	if f.RetryAfterSeconds == 0 {
		for _, seconds := range []int{detail.RetryAfterSeconds, env.RetryAfterSeconds, env.RetryAfterSnake} {
			if seconds > 0 {
				f.RetryAfterSeconds = seconds
				break
			}
		}
	}
	// Keep untrusted hints representable when converted to time.Duration.
	f.RetryAfterSeconds = min(f.RetryAfterSeconds, maxRouteRetrySeconds())
	return f
}

func maxRouteRetrySeconds() int {
	return int(min(int64(^uint(0)>>1), int64((1<<63-1)/time.Second)))
}

func cleanRoutePath(path string, redact func(string) string) string {
	path, _, _ = strings.Cut(path, "?")
	return truncate(cleanRouteDiagnostic(path, redact), 1024)
}

func cleanRouteDiagnostic(value string, redact func(string) string) string {
	value = diagnosticURL.ReplaceAllString(value, "[redacted URL]")
	value = diagnosticCredential.ReplaceAllString(value, "${1}=[redacted]")
	if redact != nil {
		return redact(value)
	}
	return logredact.String(value)
}
