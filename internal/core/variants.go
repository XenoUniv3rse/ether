package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
)

// VersionAvailability classifies how quickly a version can start playback
// without spending any indexer or provider searches.
type VersionAvailability string

const (
	VersionInstant     VersionAvailability = "instant"
	VersionCached      VersionAvailability = "cached"
	VersionFetch       VersionAvailability = "fetch"
	VersionUnavailable VersionAvailability = "unavailable"
)

// VersionProfile is the quality/source signature stored for a viewer's
// show-level episode version preference (e.g. "4K DV" or "1080p Remux").
type VersionProfile struct {
	Height       int    `json:"height,omitempty"`
	DynamicRange string `json:"dynamicRange,omitempty"` // "dv", "hdr10", "hdr10+", "hlg", "sdr"
	SourceClass  string `json:"sourceClass,omitempty"`  // "remux", "disc", "web", "other"
}

// ViewerChoiceSummary describes a viewer's remembered version choice for a
// title or its show, projected for frontends.
type ViewerChoiceSummary struct {
	Scope     string         `json:"scope"` // "title" or "show"
	Active    bool           `json:"active"`
	Label     string         `json:"label"`
	Profile   VersionProfile `json:"profile,omitempty"`
	Selector  string         `json:"selector,omitempty"`
	ReleaseID string         `json:"releaseId,omitempty"`
}

// ReleaseVariant is one playable version of a title, offered in the Plex
// "Play Version" picker: a distinct candidate release, by quality tier or
// edition (cut).
type ReleaseVariant struct {
	AutoSelectionReason AutoSelectionReason `json:"autoSelectionReason,omitempty"`
	// ID is the stable menu identity; configured slots keep it while the
	// concrete Release changes. Frontends encode it in Media/Part ids.
	ID string `json:"id"`
	// Label is the viewer-facing name, e.g. "4K Dolby Vision Remux" or
	// "1080p WEB-DL (High Compatibility)".
	Label string `json:"label"`
	// Edition is the cut, e.g. "Director's Cut", "Extended", "IMAX"; empty
	// for the theatrical/unspecified cut.
	Edition string `json:"edition,omitempty"`
	// Auto marks the resolver's own pick for this client ("Best for Device").
	Auto bool `json:"auto,omitempty"`
	// Remembered marks a variant backed by the viewer's remembered choice.
	Remembered bool `json:"remembered,omitempty"`
	// Pinned marks a variant backed by the owner's pin for this title.
	Pinned bool `json:"pinned,omitempty"`
	// Availability reports local/cached/fetch/unavailable readiness without
	// searching indexers or opening remote sources.
	Availability VersionAvailability `json:"availability,omitempty"`
	// AvailabilityNote is an optional human-readable note for Availability.
	AvailabilityNote string  `json:"availabilityNote,omitempty"`
	Release          Release `json:"release"`
	// Info is the expected media info, estimated from the release name until
	// the release has been probed.
	Info MediaInfo `json:"info"`
}

// MaxVariants caps both version-menu modes, including Auto. The resolver
// and Plex front end share this bound for tier/cut selection and balanced
// alternatives across resolutions.
const MaxVariants = 7

// VariantResolver lists distinct candidate releases of a title for a client,
// best first, without health-checking every one of them. Implemented by
// internal/resolver.
type VariantResolver interface {
	Variants(ctx context.Context, req ResolveRequest, max int) ([]ReleaseVariant, error)
}

// VariantPlayback is implemented by playback managers that can prepare a
// session on a specific release (the viewer picked a version). The release
// is health-checked like any other; when it is unavailable the manager falls
// back to the normal choice for the client.
type VariantPlayback interface {
	PrepareVariant(ctx context.Context, user *User, key RatingKey, caps ClientCaps, releaseID string) (*PlaySession, error)
}

// ExactVariantPlayback never substitutes a different release, including
// during later failover. The requested release may instead become unavailable.
type ExactVariantPlayback interface {
	PrepareExactVariant(ctx context.Context, user *User, key RatingKey, caps ClientCaps, releaseID string) (*PlaySession, error)
}

// DecisionTrace is the privacy-safe shape of the latest resolution for a
// title: ceilings, who chose, and a bounded candidate list. It has no
// username, address, or stream URL. Release names are the file names the
// resolver ranked.
type DecisionTrace struct {
	QualityFallback    string
	Profile            string
	ProfileMaxHeight   int
	ClientMaxHeight    int
	EffectiveMaxHeight int
	AllowHDR           bool
	AllowDolbyVision   bool
	Chooser            string
	ChosenVersion      string
	Candidates         []DecisionCandidate
}

// DecisionCandidate is one ranked or rejected release in a DecisionTrace.
type DecisionCandidate struct {
	ReleaseID     string
	Name          string
	Height        int
	Size          int64
	Score         float64
	Rejected      bool
	Reason        string
	Healthy       bool
	HealthChecked bool
}

// DecisionTraceSource is the latest resolution for a title. Plex reads it
// for the support capture and stays outside the resolver package.
type DecisionTraceSource interface {
	LatestDecision(key RatingKey) (DecisionTrace, bool)
}

// VersionSelectorPrefix identifies a configured menu choice rather than a
// concrete release. Only the resolver interprets or authorizes its payload.
const VersionSelectorPrefix = "ether-slot:v1:"

// KindAutoVersionID is the synthetic Auto slot identity of a plugin kind
// item's version menu (index 0). Its payload matches the canonical Auto
// selector ({"k":"auto"}), so Auto-slot checks recognize it uniformly across
// resolver, playback and frontends. Picked, Vela omits versionId.
const KindAutoVersionID = VersionSelectorPrefix + "eyJrIjoiYXV0byJ9"

// IsAutoVersionSelector reports whether selector denotes Auto (empty or an
// encoded {"k":"auto"} configured version slot).
func IsAutoVersionSelector(selector string) bool {
	if selector == "" || selector == KindAutoVersionID {
		return true
	}
	if !strings.HasPrefix(selector, VersionSelectorPrefix) || len(selector) > 256 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(selector, VersionSelectorPrefix))
	if err != nil {
		return false
	}
	var slot struct {
		Kind string `json:"k"`
	}
	return json.Unmarshal(raw, &slot) == nil && slot.Kind == "auto"
}

// IsExplicitVersionSlotSelector reports whether selector denotes a non-Auto
// configured version slot (as opposed to Auto or a legacy concrete release ID).
func IsExplicitVersionSlotSelector(selector string) bool {
	return strings.HasPrefix(selector, VersionSelectorPrefix) && !IsAutoVersionSelector(selector)
}
