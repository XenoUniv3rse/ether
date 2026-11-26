package images

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
)

const (
	Immutable  = "private, max-age=31536000, immutable"
	Revalidate = "private, no-cache"
)

func ETag(data []byte, contentType string) string {
	h := sha256.New()
	h.Write([]byte(contentType))
	h.Write([]byte{0})
	h.Write(data)
	return fmt.Sprintf("\"%x\"", h.Sum(nil))
}

// NotModified sets the representation's validator. Call after authorization.
func NotModified(w http.ResponseWriter, r *http.Request, etag string) bool {
	w.Header().Set("ETag", etag)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	for _, line := range r.Header.Values("If-None-Match") {
		for _, candidate := range strings.Split(line, ",") {
			tag := strings.TrimSpace(candidate)
			if tag == "*" || strings.TrimPrefix(tag, "W/") == etag {
				w.WriteHeader(http.StatusNotModified)
				return true
			}
		}
	}
	return false
}
