package frontend

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// AppPasswordIdentity authenticates only a dedicated, revocable app password.
// The host applies its shared, verified-peer limiter to failed authentication.
type AppPasswordIdentity interface {
	AuthenticateAppPassword(context.Context, string, string) (*Viewer, error)
}

// XtreamUsername is a generated login name, independent of household sign-in.
// Its input is a random app password; neither value reveals a viewer identifier.
func XtreamUsername(password string) string {
	sum := sha256.Sum256([]byte(password))
	return "xt_" + hex.EncodeToString(sum[:12])
}

// XtreamPasswordAlphabet avoids characters that are easy to confuse on a TV.
const XtreamPasswordAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// AppLoginTokenIdentity authenticates a dedicated app login proven by a
// salted hash, md5(password + salt) in hex, rather than the password itself.
// Failed proofs charge the same verified-peer limiter as failed passwords.
type AppLoginTokenIdentity interface {
	AuthenticateAppToken(ctx context.Context, username, token, salt string) (*Viewer, error)
}

// AppLoginPasswordLength is the generated app password length for a surface.
// Xtream keeps its short TV-typed form; other surfaces get 15 characters,
// because their clients may prove the password with an md5 token that an
// eavesdropper could attack offline.
func AppLoginPasswordLength(surface string) int {
	if surface == "xtream" {
		return 10
	}
	return 15
}

// FormatAppLoginPassword groups a generated password for display: 4-4-2 for
// the 10-character Xtream form, groups of five otherwise.
func FormatAppLoginPassword(raw string) string {
	if len(raw) == 10 {
		return raw[:4] + "-" + raw[4:8] + "-" + raw[8:]
	}
	var b strings.Builder
	for i := 0; i < len(raw); i += 5 {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(raw[i:min(i+5, len(raw))])
	}
	return b.String()
}

// AppLoginCredential converts a generated app login pair to a domain-separated
// internal grant credential. Only its hash is persisted. All equivalent
// spellings (case, group dashes) share one grant and playback slot; anything
// that is not a generated pair reports false, and long app passwords retain
// their exact bytes.
func AppLoginCredential(username, password string) (string, bool) {
	if len(username) < 3 || len(username) > 12 || len(password) > 128 {
		return "", false
	}
	username = strings.ToLower(username)
	for _, c := range username {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return "", false
		}
	}
	password = strings.ReplaceAll(strings.ToLower(password), "-", "")
	if len(password) != 10 && len(password) != 15 {
		return "", false
	}
	for _, c := range password {
		if !strings.ContainsRune(XtreamPasswordAlphabet, c) {
			return "", false
		}
	}
	sum := sha256.Sum256([]byte("ether:xtream-short:v1:" + username + ":" + password))
	return core.GrantCredentialPrefix + base64.RawURLEncoding.EncodeToString(sum[:]), true
}

// XtreamGrantCredential is AppLoginCredential for the Xtream surface.
func XtreamGrantCredential(username, password string) (string, bool) {
	return AppLoginCredential(username, password)
}

// LibraryExportPage is a bounded, policy-filtered export batch. Next advances
// over raw candidates, including those hidden by policy, so no prefix is rescanned.
type LibraryExportPage struct {
	Items []Title
	Next  RatingKey
	Done  bool
}
type LibraryExporter interface {
	ExportLibraryPage(context.Context, *Viewer, Device, string, RatingKey, int) (LibraryExportPage, error)
}

type exportTitlesOnlyKey struct{}

// WithExportTitlesOnly requests title and policy facts without row credits.
func WithExportTitlesOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, exportTitlesOnlyKey{}, true)
}

func ExportTitlesOnly(ctx context.Context) bool {
	only, _ := ctx.Value(exportTitlesOnlyKey{}).(bool)
	return only
}

// ExportPolicy is what one viewer's export of one view depends on besides
// catalogue content. Two exports with the same Stamp are built under the
// same policy; any change to the viewer's restrictions, hidden titles,
// catalogue rules or library changes it. TitleJudged reports a policy that
// narrows the export by title facts (a rating cap, share filter, blocked
// genres or keywords, section limits or the viewer's own catalogue rules):
// a catalogue write can then change what the viewer may see.
type ExportPolicy struct {
	Stamp       string
	TitleJudged bool
}

// LibraryExportPolicy reports ExportPolicy without reading the library.
type LibraryExportPolicy interface {
	ExportPolicy(context.Context, *Viewer, Device, string) (ExportPolicy, error)
}

// AppConnectionStatus reports durable login leases, including other nodes.
type AppConnectionStatus interface {
	AppConnections(context.Context, string) (int, error)
}

// AppConnectionAdmission reserves a viewer/login stream slot after primary
// URL authentication and item policy checks.
type AppConnectionAdmission interface {
	PrepareAppConnection(*http.Request, string, PlanRequest) error
}

// AppConnectionRenewal maintains an admitted slot during playback served
// outside the lazy stream transport.
type AppConnectionRenewal interface {
	RenewAppConnection(*http.Request, string, PlanRequest) (context.Context, func(), error)
}

// AppConnectionLimits returns zero for unlimited, else the effective viewer/device cap.
type AppConnectionLimits interface {
	AppConnectionLimit(context.Context, string) (int, error)
}
