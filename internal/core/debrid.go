package core

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Release protocols (Release.Protocol).
const (
	ProtocolUsenet  = ""        // the zero value: an NZB from a Newznab indexer
	ProtocolTorrent = "torrent" // a Torznab result streamed through a debrid service
	// ProtocolUnstated is a filename preview with no protocol fact.
	// Live releases keep the empty-string Usenet default; this value
	// exists so a preview cannot report that default as evidence.
	ProtocolUnstated = "unstated"
	// ProtocolHTTP is a stream URL a Stremio addon resolved itself (often
	// through the addon owner's own debrid account), read with HTTP range
	// requests. It needs no debrid account in Ether (additive). The same
	// name is Plex for Mac / Desktop's protocol=http conversion (a chunked
	// Matroska stream on /video/:/transcode/universal/start).
	ProtocolHTTP = "http"
)

// ReleaseTarget is the episode or movie wanted from a multi-file release such
// as a season pack or dual-language movie pack.
type ReleaseTarget struct {
	// CanonicalSeason/CanonicalEpisode retain TMDB identity when Season/Episode
	// select a file using the owner's alternate release numbering.
	CanonicalSeason  int `json:"canonicalSeason,omitempty"`
	CanonicalEpisode int `json:"canonicalEpisode,omitempty"`
	// Aliases lists verified alternate season/episode numbers for the same
	// canonical episode so open-time file checks accept alias-numbered files.
	Aliases []EpisodeNumber `json:"aliases,omitempty"`
	// MovieTitles and MovieYear constrain file selection inside movie collections.
	MovieTitles []string `json:"movieTitles,omitempty"`
	MovieYear   int      `json:"movieYear,omitempty"`
	// WantedLanguages and OriginalLanguage rank full-length language versions
	// inside multi-file movie torrents and NZB archives before falling back to
	// byte size (additive).
	WantedLanguages  []string `json:"wantedLanguages,omitempty"`
	OriginalLanguage string   `json:"originalLanguage,omitempty"`
	// FileIndex is a zero-based index in the complete torrent/NZB file list; the selected file must be a video unless AllowNonVideo is set.
	FileIndex *int `json:"fileIndex,omitempty"`
	// ExactFile selects a plugin-requested path or unambiguous basename.
	ExactFile string `json:"exactFile,omitempty"`
	// AllowNonVideo permits an explicit FileIndex/ExactFile, archive member
	// selector (Member/MemberIndex), or non-video MIME target to choose a
	// non-video file instead of requiring video.
	AllowNonVideo bool `json:"allowNonVideo,omitempty"`
	// Member selects an archive member path or unambiguous basename (e.g.
	// from release.fileHint or container.member) when AllowNonVideo is set.
	Member string `json:"member,omitempty"`
	// MemberIndex selects a zero-based archive member index when AllowNonVideo
	// is set (e.g. from container.memberIndex).
	MemberIndex *int `json:"memberIndex,omitempty"`
	// Track selects an audio track inside a multi-track release or archive by
	// disc/track number first, then normalised title tokens, failing on ambiguity.
	Track *TrackHint `json:"track,omitempty"`
	// MIME is the declared or kind-default media type for non-video release
	// file selection (e.g. "audio/flac", "application/epub+zip", "audio/*").
	MIME string `json:"mime,omitempty"`
	// ArchivePassword is an optional archive password resolved from a plugin
	// secret for opening encrypted Usenet archives; never serialized.
	ArchivePassword string `json:"-"`
	// FileName is the source-matched file hint, never proof of episode identity.
	FileName string `json:"fileName,omitempty"`
	// AbsoluteEpisode is the verified anime episode number across seasons.
	AbsoluteEpisode int `json:"absoluteEpisode,omitempty"`
	// Cour and CourEpisode identify the cour (part) and relative episode
	// within a multi-cour anime season pack (0 when unmapped).
	Cour          int      `json:"cour,omitempty"`
	CourEpisode   int      `json:"courEpisode,omitempty"`
	Season        int      `json:"season,omitempty"`
	Episode       int      `json:"episode,omitempty"`
	AirDate       string   `json:"airDate,omitempty"`
	ShowYear      int      `json:"showYear,omitempty"`
	EpisodeTitle  string   `json:"episodeTitle,omitempty"`
	ShowTitles    []string `json:"showTitles,omitempty"`
	SpecialTitles []string `json:"specialTitles,omitempty"`
	// AddonSpecialEpisode is the verified IMDb number before catalogue renumbering.
	AddonSpecialEpisode int `json:"addonSpecialEpisode,omitempty"`
	// PackID is the untargeted release ID of a FullSeason pack before
	// TargetReleaseID derives the per-episode ID (additive).
	PackID string `json:"packId,omitempty"`
	// ExpectedIDs identify the requested series; ReportedIDs are the
	// release's indexer-reported series IDs, retained for file validation.
	ExpectedIDs IDs `json:"expectedIds,omitempty"`
	ReportedIDs IDs `json:"reportedIds,omitempty"`
	// Anime marks an anime request: its "Part N" files are read as counted
	// within the cour unless its cour mapping says otherwise (additive).
	Anime bool `json:"anime,omitempty"`
}

// ErrWrongContent is a request-specific release mismatch, never an account
// failure or proof that the torrent is unusable for another title.
var ErrWrongContent = fmt.Errorf("release contains different content: %w", ErrNotAvailable)

// ErrNotCached reports that no debrid provider can serve a torrent release
// instantly. It is a per-release skip, never proof the release is broken,
// so it wraps ErrNotAvailable and is not a permanent failure.
var ErrNotCached = fmt.Errorf("not cached by any debrid provider: %w", ErrNotAvailable)

// PrivateTorrentReason explains why an uncached private-tracker torrent is
// skipped rather than added to or downloaded by a debrid service.
const PrivateTorrentReason = "private tracker torrent: only an existing debrid cache is used"

type privateUncachedError struct{}

func (privateUncachedError) Error() string { return PrivateTorrentReason }
func (privateUncachedError) Unwrap() error { return ErrNotCached }

// ErrPrivateUncached reports that a private-tracker torrent is not already
// cached on any debrid provider and therefore was never added or queued.
var ErrPrivateUncached error = privateUncachedError{}

// ErrDownloadQueued reports that an uncached torrent was handed to a debrid
// account to download (cached-only off): not playable yet, but it will be.
var ErrDownloadQueued = fmt.Errorf("sent to the debrid service to download: %w", ErrNotCached)

// DownloadQueuedError wraps ErrDownloadQueued with optional provider queue
// context (for example TorBox holding a download behind active slots).
type DownloadQueuedError struct {
	Provider        string
	ActiveDownloads int
	Err             error
}

func (e *DownloadQueuedError) Error() string {
	if e == nil {
		return ErrDownloadQueued.Error()
	}
	if detail := e.Detail(); detail != "" {
		if e.Err != nil {
			return fmt.Sprintf("%s: %s", detail, e.Err.Error())
		}
		return detail
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return ErrDownloadQueued.Error()
}

func (e *DownloadQueuedError) Unwrap() []error {
	if e == nil || e.Err == nil {
		return []error{ErrDownloadQueued}
	}
	return []error{e.Err, ErrDownloadQueued}
}

// Detail describes why the download is waiting in the provider's queue, or ""
// when no active-download queue count is known.
func (e *DownloadQueuedError) Detail() string {
	if e == nil || e.Provider == "" || e.ActiveDownloads <= 0 {
		return ""
	}
	unit := "active downloads"
	if e.ActiveDownloads == 1 {
		unit = "active download"
	}
	return fmt.Sprintf("queued at %s behind %d %s", e.Provider, e.ActiveDownloads, unit)
}

// DownloadQueuedDetail returns the provider queue reason (for example
// "queued at TorBox behind 3 active downloads") when err carries one, else "".
func DownloadQueuedDetail(err error) string {
	var qe *DownloadQueuedError
	if errors.As(err, &qe) && qe != nil {
		return qe.Detail()
	}
	return ""
}

// ErrDownloadRefused reports that an uncached torrent could not be sent to
// a debrid account to download (cached-only off): the service refused it
// (a blocked file, a refused add) or the add failed. Nothing is downloading.
var ErrDownloadRefused = fmt.Errorf("the debrid service did not take it to download: %w", ErrNotCached)

// ErrDownloadPaused means Ether temporarily stopped adding torrents after
// repeated provider refusals; no download was submitted during the pause.
var ErrDownloadPaused = fmt.Errorf("torrent adds are temporarily paused after provider refusals: %w", ErrDownloadRefused)

// ErrDownloadLimit is ErrDownloadRefused because the debrid account is at
// its active-download limit (Real-Debrid too_many_active_downloads, TorBox
// ACTIVE_LIMIT): no release of any title can start downloading for now.
var ErrDownloadLimit = fmt.Errorf("the debrid account is at its active-download limit: %w", ErrDownloadRefused)

// UncachedError is a resolve that failed only because no debrid account
// holds a torrent release of the title. It wraps the per-release errors and
// says what the owner's settings and other sources contributed, so a player
// message can name the real reason.
type UncachedError struct {
	Err error
	// CachedOnly is the owner's "Cached torrents only" setting at the time.
	CachedOnly bool
	// Usenet is what Usenet contributed: UsenetNone (a Usenet provider is
	// set up but the search found no Usenet release), UsenetFailed (Usenet
	// releases were found and none played), or "" (no Usenet provider).
	Usenet string
}

// UncachedError.Usenet values.
const (
	UsenetNone   = "none"
	UsenetFailed = "failed"
)

func (e *UncachedError) Error() string { return e.Err.Error() }
func (e *UncachedError) Unwrap() error { return e.Err }

// ErrNoSources reports that there is nothing to search: no enabled indexer
// at all. A configuration problem, not proof a title has no release, so
// callers must not record the title as unavailable.
var ErrNoSources = fmt.Errorf("no indexers to search: %w", ErrNotAvailable)

// ErrNoTorrentSources reports that debrid is the only way to play (no
// Usenet provider) but no enabled indexer returns torrents (Torznab, a
// Stremio addon or a torrent built-in site).
var ErrNoTorrentSources = fmt.Errorf("debrid has no torrent indexer to search: %w", ErrNoSources)

// ErrDebridAuth is a debrid service rejecting the configured API key (or an
// account that cannot use the API). A configuration problem, never a
// release failure.
var ErrDebridAuth = errors.New("debrid: authentication failed")

// TargetReleaseID derives a stable release ID for one episode of a
// multi-file release, so health, pins, cache and bad marks apply to the
// episode file rather than the whole pack.
func TargetReleaseID(releaseID string, t ReleaseTarget) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s|s%de%d", releaseID, t.Season, t.Episode)))
	return hex.EncodeToString(h[:])
}

// fileSelectionKey returns a stable hash suffix when t names an explicit file,
// archive member, or audio track inside a multi-file release, or "" when no
// explicit selector is set.
func (t ReleaseTarget) fileSelectionKey() string {
	exact := strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(t.ExactFile), "\\", "/"), "/")
	member := strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(t.Member), "\\", "/"), "/")
	var trackKey string
	if t.Track != nil && (t.Track.Disc > 0 || t.Track.Number > 0 || strings.TrimSpace(t.Track.Title) != "") {
		trackKey = fmt.Sprintf("d=%d|n=%d|t=%s", t.Track.Disc, t.Track.Number, strings.ToLower(strings.TrimSpace(t.Track.Title)))
	}
	if t.FileIndex == nil && exact == "" && member == "" && t.MemberIndex == nil && trackKey == "" {
		return ""
	}
	var b strings.Builder
	if t.FileIndex != nil {
		fmt.Fprintf(&b, "fi=%d|", *t.FileIndex)
	}
	if exact != "" {
		fmt.Fprintf(&b, "ef=%s|", strings.ToLower(exact))
	}
	if member != "" {
		fmt.Fprintf(&b, "m=%s|", strings.ToLower(member))
	}
	if t.MemberIndex != nil {
		fmt.Fprintf(&b, "mi=%d|", *t.MemberIndex)
	}
	if trackKey != "" {
		fmt.Fprintf(&b, "tr=%s|", trackKey)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// ErrDebridThrottled reports that provider admission would outlast the playback budget.
var ErrDebridThrottled = errors.New("debrid rate limit")

// DebridThrottleError identifies the provider whose cooldown blocked playback.
type DebridThrottleError struct {
	Provider string
}

func (e *DebridThrottleError) Error() string { return e.Provider + " asked Ether to slow down" }
func (e *DebridThrottleError) Unwrap() error { return ErrDebridThrottled }

type debridPlaybackDeadlineKey struct{}

// WithDebridPlaybackDeadline sets the point beyond which a Play cannot wait for API admission.
func WithDebridPlaybackDeadline(ctx context.Context, deadline time.Time) context.Context {
	if current, ok := ctx.Value(debridPlaybackDeadlineKey{}).(time.Time); ok && current.Before(deadline) {
		return ctx
	}
	return context.WithValue(ctx, debridPlaybackDeadlineKey{}, deadline)
}

// DebridPlaybackDeadline returns the earlier of the playback target and the context deadline.
func DebridPlaybackDeadline(ctx context.Context) (time.Time, bool) {
	target, _ := ctx.Value(debridPlaybackDeadlineKey{}).(time.Time)
	deadline, ok := ctx.Deadline()
	if !target.IsZero() && (!ok || target.Before(deadline)) {
		return target, true
	}
	return deadline, ok
}
