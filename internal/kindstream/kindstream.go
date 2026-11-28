// Package kindstream is Ether's unified byte-acquisition and streaming layer
// for plugin-provided content kinds (spec §4, §5, §15.1). Every plugin media
// item — comic page, ebook file, audiobook chapter, podcast episode, or video
// part — resolves through kindstream into a random-access ByteSource.
package kindstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	mimepkg "mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/archive"
	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/logredact"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/resolver"
)

// Retrieval pattern constants (spec §4.1, §4.3).
const (
	PatternFile      = "file"
	PatternRange     = "range"
	PatternDownload  = "download"
	PatternDirect    = "direct"
	PatternContainer = "container"
	PatternRelease   = "release"
)

var (
	// ErrMemberUndecodable is returned when an archive member is encrypted
	// without seekable/sequential support or uses an archive format/compression
	// method that internal/archive cannot decode sequentially. It wraps
	// core.ErrArchiveUnstreamable so frontends surface it plainly.
	ErrMemberUndecodable = fmt.Errorf("kindstream: archive member not sequentially decodable: %w", core.ErrArchiveUnstreamable)

	// ErrHostNotAllowed is returned when a source URL's host is rejected by the
	// plugin's declared network allowlist.
	ErrHostNotAllowed = errors.New("kindstream: host not in plugin allowlist")

	// ErrSizeLimitExceeded is returned when a download or range-demoted source
	// exceeds its configured maxBytes or library cache quota.
	ErrSizeLimitExceeded = errors.New("kindstream: source exceeds download size limit")

	// ErrMIMEMismatch is returned when sniffed magic bytes disagree with the
	// plugin-declared MIME at the family level (e.g. text/html error page for an
	// image/* source).
	ErrMIMEMismatch = errors.New("kindstream: sniffed MIME family disagrees with declared MIME")
)

// ByteSource is the uniform random-access stream interface produced by
// kindstream for all plugin content kinds (spec §5.1).
type ByteSource interface {
	io.ReaderAt
	Size() int64  // total byte length; always > 0 once Open returns
	Name() string // filename for Content-Disposition and container sniffing
	MIME() string // MIME type (plugin-declared, verified/refined by sniffing first 512 B)
	Close() error
}

// SourceFailure is one candidate that failed to open before a later
// candidate succeeded (or before every candidate failed).
type SourceFailure struct {
	Source core.PlayableSource
	Err    error
}

// PluginAnswer reports whether the failure points at the plugin's own
// answer (see ErrPluginAnswer) rather than at an unavailable host.
func (f SourceFailure) PluginAnswer() bool { return errors.Is(f.Err, ErrPluginAnswer) }

// DefaultMaxNonVideoReleaseGrabs bounds how many non-video Usenet release
// candidates OpenSources will fetch in a single play attempt before stopping,
// so a failing play never burns an entire page of indexer grabs (#659).
const DefaultMaxNonVideoReleaseGrabs = 3

// Deps supplies the runtime dependencies needed by Open and OpenSources
// (spec §5.1).
type Deps struct {
	// LimitRangeTransport applies the plugin manager’s shared per-origin connection limit.
	LimitRangeTransport func(http.RoundTripper) http.RoundTripper
	OpenFile            func(context.Context, core.PlayableSource) (ByteSource, error)
	HTTP                *http.Client
	Cache               *blobcache.Namespace
	OpenRelease         func(ctx context.Context, r core.ReleaseHint) (core.MediaSource, error)
	// OpenReleaseSource, when set, is preferred over OpenRelease so release
	// opening receives the full PlayableSource (MIME, Container, Release).
	OpenReleaseSource func(ctx context.Context, src core.PlayableSource) (core.MediaSource, error)
	// DefaultReleaseMIME is the kind-level fallback MIME (e.g. "audio/*",
	// "application/epub+zip") when a release source omits src.MIME.
	DefaultReleaseMIME string
	// MaxNonVideoReleaseGrabs caps how many non-video Usenet release grabs
	// OpenSources attempts per play (0 uses DefaultMaxNonVideoReleaseGrabs;
	// negative disables the cap).
	MaxNonVideoReleaseGrabs int
	// CandidateOpenTimeout, when positive, bounds how long OpenSources spends
	// opening any single candidate before moving on to the next candidate.
	CandidateOpenTimeout time.Duration
	Secret               func(ctx context.Context, name string) (string, error)
	AllowHost            func(host string) bool
	MaxDownloadBytes     int64
	// CacheWriteLimits obtains plugin download and member grants only when caching bytes.
	CacheWriteLimits func() (download, member int64)
	ItemIndex        int
	Backoff          *HostBackoff
	// OnSourceFailed, when set, is called each time a candidate fails to open,
	// regardless of whether a later candidate succeeds.
	OnSourceFailed func(attempt, total int, src core.PlayableSource, took time.Duration, err error)
	// OnSourceSkipped, when set, is called for each candidate that failed
	// before a later one opened, so the caller can log it and decide
	// whether the winner may be saved as the item's resolution.
	OnSourceSkipped func(failures []SourceFailure, winning core.PlayableSource)
	// ProbeMedia, when set, reads container metadata for an already-opened
	// ByteSource when a source opts into open-time duration verification
	// (DurationSec > 0 and DurationToleranceSec > 0). Typically wired by
	// KindResolver to consult and populate the durable release_probes cache
	// using ProbeByteSource. When nil, Open falls back directly to
	// ProbeByteSource without a cache or ffprobe fallback.
	ProbeMedia func(ctx context.Context, src core.PlayableSource, bs ByteSource) (*core.MediaInfo, error)
	// SourceRouter, when set, applies Ether's tier source routing (auto/usenet/
	// debrid, measured startup, stall demotion), grab hedge delay, and startup
	// stat recording in OpenSources. *resolver.Service implements this
	// interface directly.
	SourceRouter SourceRouter
	// SourceRelease, when set, maps a PlayableSource to the core.Release used
	// for routing, debrid-cache checks, grab detection, startup stats, and
	// delivery/stall keys (matching the Release later passed to
	// ObserveSourceStall during playback). When nil, ReleaseForSource is used.
	SourceRelease func(core.PlayableSource) core.Release
	// Title optionally supplies title metadata (runtime, popularity, votes)
	// for tier routing and delivery bitrate checks.
	Title *core.Title
}

// SourceRouter plugs Ether's resolver tier routing, grab-hedge timing, and
// startup statistics into OpenSources. *resolver.Service satisfies this
// interface directly.
type SourceRouter interface {
	RoutePlayableTier(items []resolver.PlayableRouteCandidate, title *core.Title) []int
	HealthHedgeDelay(rel core.Release) time.Duration
	ObserveSourceStartup(rel core.Release, took time.Duration)
}

// Open resolves a single core.PlayableSource into a random-access ByteSource
// according to its retrieval pattern (spec §4.3, §5.1). When the source sets
// both DurationSec and DurationToleranceSec > 0, Open verifies the container's
// running time before returning and rejects mismatches as core.ErrWrongContent
// and ErrPluginAnswer.
func Open(ctx context.Context, src core.PlayableSource, deps Deps) (ByteSource, error) {
	bs, err := openSource(ctx, src, deps)
	if err != nil {
		return nil, logredact.SourceError(err)
	}
	if err := verifySourceDuration(ctx, src, bs, deps); err != nil {
		_ = bs.Close()
		return nil, logredact.SourceError(err)
	}
	return bs, nil
}

// NormalizeNZBSource recognizes legacy NZB locators before network routing.
func NormalizeNZBSource(src core.PlayableSource) core.PlayableSource {
	locator, legacy := strings.CutPrefix(strings.TrimSpace(src.URL), "nzb:")
	if !legacy && !strings.EqualFold(strings.TrimSpace(src.Pattern), "nzb") {
		return src
	}
	if !legacy {
		locator = strings.TrimSpace(src.URL)
	}
	if locator != "" {
		hint := core.ReleaseHint{}
		if src.Release != nil {
			hint = *src.Release
		}
		hint.NZBURL = locator
		src.Release = &hint
	}
	src.Pattern, src.URL = PatternRelease, ""
	return src
}

func openSource(ctx context.Context, src core.PlayableSource, deps Deps) (ByteSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	src = NormalizeNZBSource(src)
	pattern := strings.ToLower(strings.TrimSpace(src.Pattern))
	if pattern == "" {
		if src.Container != nil {
			pattern = PatternContainer
		} else if src.Release != nil {
			pattern = PatternRelease
		} else {
			pattern = PatternRange
		}
	}

	// Fast path: if BlobRef is already populated on a non-container source, open
	// directly from the library's blobcache namespace.
	if strings.TrimSpace(src.BlobRef) != "" && pattern != PatternContainer && pattern != PatternFile {
		if deps.Cache == nil {
			return nil, errors.New("kindstream: source specifies blobRef but Cache is nil")
		}
		cached, err := deps.Cache.Open(blobcache.Ref(strings.TrimSpace(src.BlobRef)))
		if err != nil {
			return nil, err
		}
		d := digestsOf(src)
		if err := d.checkSize(cached.Size()); err != nil {
			_ = cached.Close()
			return nil, err
		}
		if d.hasHash() {
			if err := d.verifyReaderAt(cached, cached.Size()); err != nil {
				_ = cached.Close()
				return nil, err
			}
		}
		return finalizeSourceMIME(cached, src.MIME, src.ID)
	}

	switch pattern {
	case PatternFile:
		if deps.OpenFile == nil {
			return nil, core.ErrSourceUnavailable
		}
		return deps.OpenFile(ctx, src)
	case PatternRange:
		return openRangeSource(ctx, src, deps)
	case PatternDownload:
		return openDownloadSource(ctx, src, deps)
	case PatternDirect:
		return openDirectSource(ctx, src, deps)
	case PatternContainer:
		if src.Container == nil {
			return nil, errors.New("kindstream: container pattern requires core.ContainerRef")
		}
		bs, err := OpenContainer(ctx, *src.Container, src.MIME, src.MaxBytes, deps)
		if err != nil {
			return nil, err
		}
		keySum := sha256.Sum256([]byte(containerExtractCacheKey(*src.Container, bs.Name(), 0, deps.ItemIndex)))
		cacheKey := "container-" + hex.EncodeToString(keySum[:12]) + "-" + bs.Name()
		return applyDigests(ctx, bs, digestsOf(src), src.MaxBytes, cacheKey, deps)
	case PatternRelease:
		return openReleaseSource(ctx, src, deps)
	default:
		return nil, fmt.Errorf("kindstream: unknown retrieval pattern %q", src.Pattern)
	}
}

func isNonVideoUsenetRelease(cand core.PlayableSource, defaultMIME string) bool {
	cand = NormalizeNZBSource(cand)
	if strings.TrimSpace(cand.BlobRef) != "" {
		return false
	}
	pattern := strings.ToLower(strings.TrimSpace(cand.Pattern))
	if pattern == "" && cand.Release != nil && cand.Container == nil {
		pattern = PatternRelease
	}
	if pattern != PatternRelease || cand.Release == nil {
		return false
	}
	effMIME := nzb.NormalizeMIME(cand.MIME)
	if effMIME == "" {
		effMIME = nzb.NormalizeMIME(defaultMIME)
	}
	if strings.HasPrefix(effMIME, "video/") {
		return false
	}
	explicitFile := hasExplicitReleaseSelector(*cand.Release)
	archiveMember := strings.TrimSpace(cand.Release.FileHint) != "" ||
		(cand.Release.Track != nil && (cand.Release.Track.Number > 0 || strings.TrimSpace(cand.Release.Track.Title) != "")) ||
		(cand.Container != nil && (strings.TrimSpace(cand.Container.Member) != "" || cand.Container.MemberIndex != nil))
	if !nzb.IsNonVideoMIME(effMIME) && !explicitFile && !archiveMember {
		return false
	}

	r := cand.Release
	hasNZB := r.NZBURL != "" || r.NZBRef != "" || (r.Candidate != nil && (r.Candidate.NZBURL != "" || r.Candidate.NZBRef != ""))
	hasTorrent := strings.TrimSpace(r.InfoHash) != "" || strings.TrimSpace(r.Magnet) != "" || strings.TrimSpace(r.TorrentURL) != "" ||
		(r.Candidate != nil && (strings.TrimSpace(r.Candidate.InfoHash) != "" || strings.TrimSpace(r.Candidate.Magnet) != "" || strings.TrimSpace(r.Candidate.TorrentURL) != ""))
	return hasNZB && !hasTorrent
}

func effectivePattern(src core.PlayableSource) string {
	pattern := strings.ToLower(strings.TrimSpace(src.Pattern))
	if pattern == "" {
		if src.Container != nil {
			return PatternContainer
		}
		if src.Release != nil {
			return PatternRelease
		}
		return PatternRange
	}
	return pattern
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// ReleaseForSource builds the default core.Release metadata for a
// PlayableSource (protocol, ID, infohash, magnet, NZB URL, size, seeders) used
// by OpenSources for grab detection, tier routing, startup stats, and delivery
// keys when Deps.SourceRelease is not supplied.
func ReleaseForSource(src core.PlayableSource) core.Release {
	rel := core.Release{
		ID:   strings.TrimSpace(src.ID),
		Name: strings.TrimSpace(src.Label),
		Size: src.Size,
	}
	if effectivePattern(src) != PatternRelease || src.Release == nil || strings.TrimSpace(src.BlobRef) != "" {
		rel.Protocol = core.ProtocolHTTP
		if rel.ID == "" {
			rel.ID = firstNonEmptyStr(src.URL, src.Path, src.BlobRef)
		}
		return rel
	}
	r := src.Release
	var torrentURL string
	if r.Candidate != nil {
		rel.InfoHash = strings.ToLower(strings.TrimSpace(r.Candidate.InfoHash))
		rel.MagnetURL = strings.TrimSpace(r.Candidate.Magnet)
		torrentURL = strings.TrimSpace(r.Candidate.TorrentURL)
		rel.NZBURL = strings.TrimSpace(r.Candidate.NZBURL)
		rel.NZBRef = r.Candidate.NZBRef
		rel.TorrentRef = r.Candidate.TorrentRef
		if rel.Name == "" {
			rel.Name = strings.TrimSpace(r.Candidate.Title)
		}
		if rel.Size <= 0 {
			rel.Size = r.Candidate.Size
		}
		rel.Seeders = r.Candidate.Seeders
		rel.Indexer = r.Candidate.Indexer
		rel.IndexerID = r.Candidate.IndexerID
	}
	if h := strings.ToLower(strings.TrimSpace(r.InfoHash)); h != "" {
		rel.InfoHash = h
	}
	if m := strings.TrimSpace(r.Magnet); m != "" {
		rel.MagnetURL = m
	}
	if t := strings.TrimSpace(r.TorrentURL); t != "" {
		torrentURL = t
	}
	if r.TorrentRef != "" {
		rel.TorrentRef = r.TorrentRef
	}
	if r.NZBRef != "" {
		rel.NZBRef = r.NZBRef
	}
	if u := strings.TrimSpace(r.NZBURL); u != "" {
		rel.NZBURL = u
	}
	if strings.HasPrefix(strings.ToLower(torrentURL), "magnet:") {
		if rel.MagnetURL == "" {
			rel.MagnetURL = torrentURL
		}
		torrentURL = ""
	}
	if rel.InfoHash == "" && rel.MagnetURL != "" {
		if h := newznab.InfoHashFromMagnet(rel.MagnetURL); h != "" {
			rel.InfoHash = h
		}
	}
	switch {
	case rel.InfoHash != "" || rel.MagnetURL != "" || torrentURL != "" || rel.TorrentRef != "":
		rel.Protocol = core.ProtocolTorrent
		if torrentURL != "" {
			rel.NZBURL = torrentURL
		} else {
			rel.NZBURL = ""
		}
	case rel.NZBURL != "" || rel.NZBRef != "":
		rel.Protocol = core.ProtocolUsenet
	default:
		rel.Protocol = core.ProtocolHTTP
	}
	if rel.ID == "" {
		rel.ID = firstNonEmptyStr(rel.InfoHash, rel.NZBRef, rel.TorrentRef, rel.MagnetURL)
	}
	return rel
}

func isSourceKnownCached(src core.PlayableSource) bool {
	return src.Release != nil && src.Release.Candidate != nil &&
		(src.Release.Candidate.Cached || len(src.Release.Candidate.CachedBy) > 0)
}

func sourceSpendsGrab(src core.PlayableSource, rel core.Release) bool {
	if effectivePattern(src) != PatternRelease || src.Release == nil || strings.TrimSpace(src.BlobRef) != "" {
		return false
	}
	return resolver.SpendsGrab(rel)
}

func isReleaseStartupCandidate(src core.PlayableSource, rel core.Release) bool {
	return effectivePattern(src) == PatternRelease &&
		src.Release != nil &&
		strings.TrimSpace(src.BlobRef) == "" &&
		(rel.Protocol == core.ProtocolUsenet || rel.Protocol == core.ProtocolTorrent)
}

func sourcesEquivalent(a, b core.PlayableSource) bool {
	if a.Tier != nil && b.Tier != nil {
		return *a.Tier == *b.Tier
	}
	return a.Tier == nil && b.Tier == nil
}

// routeTieredSources reorders contiguous runs of sources sharing a non-nil
// Tier using Ether's resolver routing (auto/usenet/debrid, measured startup,
// and stall demotion). Untiered sources (Tier == nil) keep exact plugin order,
// and distinct tiers never cross boundaries.
func routeTieredSources(sources []core.PlayableSource, router SourceRouter, relFor func(core.PlayableSource) core.Release, title *core.Title) []core.PlayableSource {
	if len(sources) < 2 || router == nil {
		return sources
	}
	var out []core.PlayableSource
	for i := 0; i < len(sources); {
		if sources[i].Tier == nil {
			if out != nil {
				out = append(out, sources[i])
			}
			i++
			continue
		}
		tier := *sources[i].Tier
		j := i + 1
		for j < len(sources) && sources[j].Tier != nil && *sources[j].Tier == tier {
			j++
		}
		if j-i < 2 {
			if out != nil {
				out = append(out, sources[i])
			}
			i = j
			continue
		}
		if out == nil {
			out = make([]core.PlayableSource, 0, len(sources))
			out = append(out, sources[:i]...)
		}
		run := sources[i:j]
		cands := make([]resolver.PlayableRouteCandidate, len(run))
		tierTitle := title
		for k, src := range run {
			cands[k] = resolver.PlayableRouteCandidate{
				Release:     relFor(src),
				KnownCached: isSourceKnownCached(src),
			}
			if (tierTitle == nil || tierTitle.Runtime <= 0) && src.DurationSec > 0 {
				var t core.Title
				if tierTitle != nil {
					t = *tierTitle
				}
				t.Runtime = time.Duration(src.DurationSec * float64(time.Second))
				tierTitle = &t
			}
		}
		order := router.RoutePlayableTier(cands, tierTitle)
		if len(order) != len(run) {
			out = append(out, run...)
		} else {
			for _, idx := range order {
				out = append(out, run[idx])
			}
		}
		i = j
	}
	if out == nil {
		return sources
	}
	return out
}

// OpenSources opens candidate core.PlayableSources using Ether's ranked
// sliding window (resolver.RunRankedWindow): two opens stay in flight in rank
// order, failing opens advance immediately, equivalent later sources may win
// after resolver.RankedSourceGrace, two confirmed failures widen to four for
// equivalent candidates, and grab-spending opens join a single healthy-sofar
// in-flight open only as a hedge after the resolver's health hedge delay.
// Losing opens are cancelled and their ByteSources closed before returning.
//
// When Deps.SourceRouter is set, contiguous sources sharing a non-nil Tier are
// reordered with Ether's source routing (auto/usenet/debrid, measured startup,
// stall demotion), and winning release open durations feed back into the shared
// startup statistics. Sources without a Tier keep exact plugin order.
//
// When every candidate fails with core.ErrArchiveUnstreamable, that error is
// preserved rather than masked as core.ErrSourceUnavailable; when every
// candidate fails with a permanent rejection (e.g. core.ErrReleaseRejected,
// archive.ErrMissingVolume, core.ErrNoUsableFile, core.ErrArticleMissing), the
// returned error wraps core.ErrReleaseRejected rather than
// core.ErrSourceUnavailable; otherwise all-failed candidates wrap
// core.ErrSourceUnavailable (spec §4.5).
//
// For non-video Usenet release candidates (#659), OpenSources caps grabs per
// play (DefaultMaxNonVideoReleaseGrabs) so a failing play never burns a full
// page of indexer grabs, while per-release content failures (such as a missing
// track, missing archive member, missing articles, or an archive with no usable
// file) still advance to later releases.
func OpenSources(ctx context.Context, sources []core.PlayableSource, deps Deps) (ByteSource, core.PlayableSource, error) {
	if len(sources) == 0 {
		return nil, core.PlayableSource{}, core.ErrSourceUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, core.PlayableSource{}, err
	}
	relFor := deps.SourceRelease
	if relFor == nil {
		relFor = ReleaseForSource
	}
	ordered := routeTieredSources(sources, deps.SourceRouter, relFor, deps.Title)

	maxUsenetGrabs := deps.MaxNonVideoReleaseGrabs
	if maxUsenetGrabs == 0 {
		maxUsenetGrabs = DefaultMaxNonVideoReleaseGrabs
	}

	rels := make([]core.Release, len(ordered))
	grabs := make([]bool, len(ordered))
	nonVideoUsenet := make([]bool, len(ordered))
	for i, cand := range ordered {
		rels[i] = relFor(cand)
		grabs[i] = sourceSpendsGrab(cand, rels[i])
		nonVideoUsenet[i] = isNonVideoUsenetRelease(cand, deps.DefaultReleaseMIME)
	}

	type sourceAttempt struct {
		bs   ByteSource
		err  error
		took time.Duration
	}
	attempts := make([]*sourceAttempt, len(ordered))
	skipErrs := make([]error, len(ordered))
	usenetNonVideoGrabs := 0

	outcome := resolver.RunRankedWindow(ctx, resolver.RankedWindowConfig[sourceAttempt]{
		Count:     len(ordered),
		Width:     2,
		WideWidth: 4,
		Grace:     resolver.RankedSourceGrace,
		Run: func(workCtx context.Context, i int, _ bool) sourceAttempt {
			candCtx := workCtx
			var candCancel context.CancelFunc
			if deps.CandidateOpenTimeout > 0 {
				candCtx, candCancel = context.WithTimeout(workCtx, deps.CandidateOpenTimeout)
			}
			attemptStart := time.Now()
			bs, err := Open(candCtx, ordered[i], deps)
			took := time.Since(attemptStart)
			if candCancel != nil {
				candCancel()
			}
			return sourceAttempt{bs: bs, err: err, took: took}
		},
		Succeeded: func(a sourceAttempt) bool {
			return a.err == nil && a.bs != nil
		},
		ConfirmedFailure: func(a sourceAttempt) bool {
			return isConfirmedSourceFailure(a.err)
		},
		Equivalent: func(lead, other int) bool {
			return sourcesEquivalent(ordered[lead], ordered[other])
		},
		SpendsGrab: func(i int) bool {
			return grabs[i]
		},
		HedgeDelay: func(i int) time.Duration {
			if deps.SourceRouter != nil {
				return deps.SourceRouter.HealthHedgeDelay(rels[i])
			}
			return resolver.DefaultHealthHedgeDelay
		},
		BeforeLaunch: func(_, next, inflight int, _ time.Time, _ bool) (resolver.LaunchAction, time.Time) {
			if nonVideoUsenet[next] && maxUsenetGrabs > 0 && usenetNonVideoGrabs >= maxUsenetGrabs {
				// A refused grab does not consume the cap. Wait for
				// in-flight attempts to settle before skipping the next.
				if inflight > 0 {
					return resolver.LaunchHold, time.Time{}
				}
				skipErrs[next] = fmt.Errorf("source %q (%s): skipped after non-video Usenet grab cap (%d)", logredact.Source(ordered[next].ID), logredact.Source(ordered[next].Pattern), maxUsenetGrabs)
				return resolver.LaunchSkip, time.Time{}
			}
			return resolver.LaunchAllow, time.Time{}
		},
		OnLaunch: func(i int) {
			if nonVideoUsenet[i] {
				usenetNonVideoGrabs++
			}
		},
		OnResult: func(i int, a sourceAttempt) {
			var refusal *newznab.GrabRefusal
			if nonVideoUsenet[i] && errors.As(a.err, &refusal) {
				usenetNonVideoGrabs--
			}
			attempts[i] = &a
		},
		OnDrop: func(_ int, a sourceAttempt, _ bool) {
			if a.bs != nil {
				_ = a.bs.Close()
			}
		},
	})

	if outcome.Err != nil {
		return nil, core.PlayableSource{}, outcome.Err
	}
	if outcome.Winner >= 0 {
		winIdx := outcome.Winner
		winning := ordered[winIdx]
		if deps.SourceRouter != nil && isReleaseStartupCandidate(winning, rels[winIdx]) {
			deps.SourceRouter.ObserveSourceStartup(rels[winIdx], max(outcome.Result.took, time.Microsecond))
		}
		var failures []SourceFailure
		for i := 0; i < winIdx; i++ {
			a := attempts[i]
			if a == nil || a.err == nil {
				continue
			}
			if deps.OnSourceFailed != nil {
				deps.OnSourceFailed(i+1, len(ordered), ordered[i], a.took, a.err)
			}
			failures = append(failures, SourceFailure{Source: ordered[i], Err: a.err})
		}
		if len(failures) > 0 && deps.OnSourceSkipped != nil {
			deps.OnSourceSkipped(failures, winning)
		}
		return outcome.Result.bs, winning, nil
	}

	var errs []error
	allUnstreamable := true
	allRejected := true
	allWrongContent := true
	var nextGrab *newznab.GrabRefusal
	allGrabRefused := true
	for i, cand := range ordered {
		if skipErrs[i] != nil {
			errs = append(errs, skipErrs[i])
			allGrabRefused = false
			allUnstreamable = false
			allRejected = false
			allWrongContent = false
			continue
		}
		a := attempts[i]
		if a == nil || a.err == nil {
			continue
		}
		if deps.OnSourceFailed != nil {
			deps.OnSourceFailed(i+1, len(ordered), cand, a.took, a.err)
		}
		if !errors.Is(a.err, core.ErrArchiveUnstreamable) {
			allUnstreamable = false
		}
		if !isPermanentRejection(a.err) {
			allRejected = false
		}
		if !errors.Is(a.err, core.ErrWrongContent) {
			allWrongContent = false
		}
		var refusal *newznab.GrabRefusal
		if errors.As(a.err, &refusal) {
			if nextGrab == nil || refusal.RetryAt.Before(nextGrab.RetryAt) {
				nextGrab = refusal
			}
		} else {
			allGrabRefused = false
		}
		errs = append(errs, fmt.Errorf("source %q (%s): %w", logredact.Source(cand.ID), logredact.Source(cand.Pattern), a.err))
	}
	if allGrabRefused && nextGrab != nil {
		return nil, core.PlayableSource{}, nextGrab
	}
	joined := errors.Join(errs...)
	if errors.Is(joined, core.ErrBudgetExhausted) {
		// Individual attempts already report diagnostic details. Keep the
		// typed quota failure when all alternatives have failed.
		return nil, core.PlayableSource{}, core.ErrBudgetExhausted
	}
	if allUnstreamable && joined != nil {
		return nil, core.PlayableSource{}, joined
	}
	if allRejected && joined != nil {
		if errors.Is(joined, core.ErrReleaseRejected) {
			return nil, core.PlayableSource{}, joined
		}
		return nil, core.PlayableSource{}, fmt.Errorf("%w: %w", core.ErrReleaseRejected, joined)
	}
	if allWrongContent && joined != nil {
		return nil, core.PlayableSource{}, joined
	}
	return nil, core.PlayableSource{}, fmt.Errorf("%w: %v", core.ErrSourceUnavailable, joined)
}

// isConfirmedSourceFailure reports whether a candidate open failure is a
// definitive per-source rejection (not a timeout, cancellation, or transient
// provider error) that counts toward widening the ranked window from 2 to 4.
func isConfirmedSourceFailure(err error) bool {
	if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var refusal *newznab.GrabRefusal
	return errors.As(err, &refusal) || isPermanentRejection(err) ||
		errors.Is(err, core.ErrWrongContent) ||
		errors.Is(err, ErrPluginAnswer) ||
		errors.Is(err, ErrDigestMismatch) ||
		errors.Is(err, ErrMIMEMismatch) ||
		errors.Is(err, ErrSizeLimitExceeded) ||
		errors.Is(err, core.ErrNotCached)
}

func isPermanentRejection(err error) bool {
	for _, target := range []error{
		core.ErrReleaseRejected,
		core.ErrArchiveUnstreamable,
		core.ErrNoUsableFile,
		core.ErrArticleMissing,
		core.ErrArticleCorrupt,
		archive.ErrMissingVolume,
		archive.ErrCorrupt,
		archive.ErrCompressed,
		archive.ErrEncrypted,
		archive.ErrSolid,
		archive.ErrUnsupported,
		nzb.ErrInvalid,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func openRangeSource(ctx context.Context, src core.PlayableSource, deps Deps) (ByteSource, error) {
	tmpl, err := ParseURLTemplate(src.URL, deps.AllowHost)
	if err != nil {
		return nil, err
	}
	redirectPatterns, err := validateRedirectHeaderPatterns(src.KeepHeadersOnRedirect, deps.AllowHost)
	if err != nil {
		return nil, err
	}
	hdrs, err := ExpandHeaders(ctx, src.Headers, deps.Secret)
	if err != nil {
		return nil, err
	}
	knownSize := src.Size
	if src.SizeApproximate {
		knownSize = 0
	}
	if src.Connections < 0 || src.Connections > 6 {
		return nil, fmt.Errorf("kindstream: range connections must be 1 to 6")
	}
	client := deps.HTTP
	if src.Connections > 1 && deps.LimitRangeTransport != nil {
		if client == nil {
			client = defaultStreamingHTTPClient()
		}
		limited := *client
		limited.Transport = deps.LimitRangeTransport(client.Transport)
		client = &limited
	}
	hr, err := newHTTPReaderAt(ctx, httpReaderConfig{
		Connections:           src.Connections,
		Template:              tmpl,
		Headers:               hdrs,
		SecretHeaderKeys:      secretHeaderNames(src.Headers),
		KeepHeadersOnRedirect: redirectPatterns,
		ItemIndex:             deps.ItemIndex,
		KnownSize:             knownSize,
		MIME:                  src.MIME,
		Client:                client,
		AllowHost:             deps.AllowHost,
		Backoff:               deps.Backoff,
	})
	if err == nil {
		if derr := digestsOf(src).checkSize(hr.Size()); derr != nil {
			_ = hr.Close()
			return nil, derr
		}
		resolvedMIME, merr := verifyAndResolveMIME(hr.SniffBytes(), src.MIME, hr.Name())
		if merr != nil {
			_ = hr.Close()
			return nil, merr
		}
		hr.mime = resolvedMIME
		return hr, nil
	}

	var noRange *noRangeSupportError
	if errors.As(err, &noRange) {
		defer noRange.closeBody()
		// Demote range -> download when within maxBytes (spec §4.3, §5.2).
		return downloadFromNoRangeResponse(ctx, src.URL, noRange, digestsOf(src), src.MIME, src.MaxBytes, deps)
	}
	return nil, err
}

func downloadFromNoRangeResponse(
	ctx context.Context,
	rawURL string,
	noRange *noRangeSupportError,
	digests expectedDigests,
	declaredMIME string,
	maxBytes int64,
	deps Deps,
) (ByteSource, error) {
	limit := effectiveDownloadMaxBytes(maxBytes, deps)
	if limit > 0 && noRange.size > limit {
		return nil, fmt.Errorf("%w: upstream returned 200 without Range support and size %d > limit %d", ErrSizeLimitExceeded, noRange.size, limit)
	}
	resolvedMIME, err := verifyAndResolveMIME(noRange.head, declaredMIME, defaultNameFromURL(rawURL))
	if err != nil {
		return nil, err
	}

	combined := io.MultiReader(bytes.NewReader(noRange.head), noRange.resp.Body)
	if limit > 0 {
		combined = &limitEnforcingReader{r: combined, max: limit}
	}

	cacheKey := downloadCacheKey(rawURL, deps.ItemIndex)
	if deps.Cache != nil {
		expectedSize := int64(-1)
		if noRange.size > 0 {
			expectedSize = noRange.size
		}
		ref, err := deps.Cache.PutWithExpectedSHA256(ctx, cacheKey, newDigestVerifyReader(combined, digests), expectedSize, digests.sha256, resolvedMIME)
		if err != nil {
			if errors.Is(err, blobcache.ErrTooLarge) {
				return nil, fmt.Errorf("%w: %v", ErrSizeLimitExceeded, err)
			}
			return nil, err
		}
		cached, err := deps.Cache.Open(ref)
		if err != nil {
			return nil, err
		}
		return &renamedByteSource{
			ByteSource: cached,
			name:       defaultNameFromURL(rawURL),
			mime:       resolvedMIME,
		}, nil
	}

	return readIntoMemoryByteSource(combined, limit, noRange.size, digests, defaultNameFromURL(rawURL), resolvedMIME)
}

func openDownloadSource(ctx context.Context, src core.PlayableSource, deps Deps) (ByteSource, error) {
	limit := effectiveDownloadMaxBytes(src.MaxBytes, deps)
	if limit > 0 && src.Size > limit {
		return nil, fmt.Errorf("%w: declared size %d exceeds limit %d", ErrSizeLimitExceeded, src.Size, limit)
	}

	tmpl, err := ParseURLTemplate(src.URL, deps.AllowHost)
	if err != nil {
		return nil, err
	}
	redirectPatterns, err := validateRedirectHeaderPatterns(src.KeepHeadersOnRedirect, deps.AllowHost)
	if err != nil {
		return nil, err
	}
	targetURL, err := tmpl.Expand(TemplateVars{ItemIndex: deps.ItemIndex}, deps.AllowHost)
	if err != nil {
		return nil, err
	}
	hdrs, err := ExpandHeaders(ctx, src.Headers, deps.Secret)
	if err != nil {
		return nil, err
	}
	secretKeys := secretHeaderNames(src.Headers)

	cacheKey := downloadCacheKey(targetURL, deps.ItemIndex)
	name := defaultNameFromURL(targetURL)

	fetchStream := func(ctx context.Context) (*http.Response, error) {
		baseClient := deps.HTTP
		if baseClient == nil {
			baseClient = defaultStreamingHTTPClient()
		}
		backoff := deps.Backoff
		if backoff == nil {
			backoff = defaultBackoff
		}
		if err := backoff.wait(ctx, targetURL); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
		client := wrapRedirectClient(baseClient, req.URL, req.Header, secretKeys, redirectPatterns, deps.AllowHost)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			backoff.onRateLimit(targetURL, resp.Header.Get("Retry-After"))
			_ = resp.Body.Close()
			return nil, fmt.Errorf("kindstream: download HTTP %d", resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("kindstream: download HTTP %d", resp.StatusCode)
		}
		backoff.onSuccess(targetURL)
		return resp, nil
	}

	if deps.Cache != nil {
		ref, err := deps.Cache.SingleFlightPut(ctx, cacheKey, func(ctx context.Context) (blobcache.Ref, error) {
			resp, err := fetchStream(ctx)
			if err != nil {
				return "", err
			}
			defer resp.Body.Close()

			expectedSize := int64(-1)
			if src.Size > 0 && !src.SizeApproximate {
				expectedSize = src.Size
			} else if resp.ContentLength > 0 {
				expectedSize = resp.ContentLength
			}
			if limit > 0 && expectedSize > limit {
				return "", fmt.Errorf("%w: Content-Length %d exceeds limit %d", ErrSizeLimitExceeded, expectedSize, limit)
			}

			var r io.Reader = resp.Body
			if limit > 0 {
				r = &limitEnforcingReader{r: resp.Body, max: limit}
			}
			ref, err := deps.Cache.PutWithExpectedSHA256(ctx, cacheKey, newDigestVerifyReader(r, digestsOf(src)), expectedSize, src.SHA256, src.MIME)
			if err != nil {
				if errors.Is(err, blobcache.ErrTooLarge) {
					return "", fmt.Errorf("%w: %v", ErrSizeLimitExceeded, err)
				}
				return "", err
			}
			return ref, nil
		})
		if err != nil {
			return nil, err
		}
		cached, err := deps.Cache.Open(ref)
		if err != nil {
			return nil, err
		}
		return finalizeSourceMIME(&renamedByteSource{
			ByteSource: cached,
			name:       name,
			mime:       src.MIME,
		}, src.MIME, name)
	}

	resp, err := fetchStream(ctx)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	expectedSize := int64(0)
	if src.Size > 0 && !src.SizeApproximate {
		expectedSize = src.Size
	} else if resp.ContentLength > 0 {
		expectedSize = resp.ContentLength
	}
	return readIntoMemoryByteSource(resp.Body, limit, expectedSize, digestsOf(src), name, src.MIME)
}

// DirectSource represents a validated "direct" core.PlayableSource. Callers that
// support client redirects (spec §4.3) can extract the target URL via
// DirectURL() and issue a 307 Temporary Redirect without proxying bytes; callers
// that require a ByteSource stream (such as tests or local transcode bridges)
// can call ReadAt, which lazily opens the underlying HTTP source.
type DirectSource struct {
	ctx     context.Context
	src     core.PlayableSource
	deps    Deps
	url     string
	headers map[string]string
	name    string
	mime    string

	mu       sync.Mutex
	delegate ByteSource
}

// DirectURL returns the validated, template-expanded target URL for a 307 redirect.
func (d *DirectSource) DirectURL() string { return d.url }

// DirectHeaders returns the resolved request headers for the direct URL.
func (d *DirectSource) DirectHeaders() map[string]string { return d.headers }

// AsDirectSource extracts the *DirectSource if bs was opened from a "direct"
// core.PlayableSource.
func AsDirectSource(bs ByteSource) (*DirectSource, bool) {
	d, ok := bs.(*DirectSource)
	return d, ok
}

func openDirectSource(ctx context.Context, src core.PlayableSource, deps Deps) (ByteSource, error) {
	tmpl, err := ParseURLTemplate(src.URL, deps.AllowHost)
	if err != nil {
		return nil, err
	}
	if _, err := validateRedirectHeaderPatterns(src.KeepHeadersOnRedirect, deps.AllowHost); err != nil {
		return nil, err
	}
	expandedURL, err := tmpl.Expand(TemplateVars{ItemIndex: deps.ItemIndex}, deps.AllowHost)
	if err != nil {
		return nil, err
	}
	hdrs, err := ExpandHeaders(ctx, src.Headers, deps.Secret)
	if err != nil {
		return nil, err
	}
	mime := strings.TrimSpace(src.MIME)
	if mime == "" || mime == "application/octet-stream" {
		// A direct link without a declared type gets one from its file
		// extension (audio/mpeg for .mp3, application/epub+zip for .epub)
		// so clients receive a usable Content-Type.
		if u, perr := url.Parse(expandedURL); perr == nil {
			if t := mimepkg.TypeByExtension(strings.ToLower(path.Ext(u.Path))); t != "" {
				mime = strings.TrimSpace(strings.SplitN(t, ";", 2)[0])
			}
		}
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	return &DirectSource{
		ctx:     context.WithoutCancel(ctx),
		src:     src,
		deps:    deps,
		url:     expandedURL,
		headers: hdrs,
		name:    defaultNameFromURL(expandedURL),
		mime:    mime,
	}, nil
}

func (d *DirectSource) ensureDelegate() (ByteSource, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.delegate != nil {
		return d.delegate, nil
	}
	cp := d.src
	cp.Pattern = PatternRange
	openCtx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
	defer cancel()
	bs, err := openRangeSource(openCtx, cp, d.deps)
	if err != nil {
		return nil, err
	}
	d.delegate = bs
	return bs, nil
}

func (d *DirectSource) ReadAt(p []byte, off int64) (int, error) {
	bs, err := d.ensureDelegate()
	if err != nil {
		return 0, err
	}
	return bs.ReadAt(p, off)
}

func (d *DirectSource) Size() int64 {
	if d.src.Size > 0 && !d.src.SizeApproximate {
		return d.src.Size
	}
	if bs, err := d.ensureDelegate(); err == nil {
		return bs.Size()
	}
	return 0
}

func (d *DirectSource) Name() string { return d.name }
func (d *DirectSource) MIME() string { return d.mime }

func (d *DirectSource) Close() error {
	d.mu.Lock()
	del := d.delegate
	d.delegate = nil
	d.mu.Unlock()
	if del != nil {
		return del.Close()
	}
	return nil
}

func openReleaseSource(ctx context.Context, src core.PlayableSource, deps Deps) (ByteSource, error) {
	if src.Release == nil {
		return nil, errors.New("kindstream: release pattern requires core.ReleaseHint")
	}
	var (
		ms  core.MediaSource
		err error
	)
	if deps.OpenReleaseSource != nil {
		ms, err = deps.OpenReleaseSource(ctx, src)
	} else if deps.OpenRelease != nil {
		ms, err = deps.OpenRelease(ctx, *src.Release)
	} else {
		return nil, errors.New("kindstream: OpenRelease dependency is not configured")
	}
	if err != nil {
		if hasExplicitReleaseOrMemberSelector(src) && errors.Is(err, core.ErrWrongContent) {
			// The plugin named a file or archive member the release does not
			// contain as named: its answer is wrong, the release is not down.
			return nil, fmt.Errorf("%w: %w", ErrPluginAnswer, err)
		}
		return nil, err
	}

	effMIME := strings.TrimSpace(src.MIME)
	if effMIME == "" {
		effMIME = strings.TrimSpace(deps.DefaultReleaseMIME)
	}

	// Compressed or solid multi-volume non-video archives planned by
	// stream.Engine hand over their raw volumes so openContainerOverVolumes
	// decodes them through the shared internal/archive stack into deps.Cache.
	if avs, ok := ms.(core.ArchiveVolumeSource); ok {
		rawVols, fmtHint, member := avs.ArchiveVolumes()
		if len(rawVols) == 0 {
			_ = ms.Close()
			return nil, fmt.Errorf("kindstream: archive volume source has no volumes: %w", core.ErrNoUsableFile)
		}
		vols := make([]archive.Volume, len(rawVols))
		for i, rv := range rawVols {
			vols[i] = archive.Volume{R: rv, Size: rv.Size()}
		}
		pwSecret := src.Release.PasswordSecret
		var memberIdx *int
		if src.Container != nil {
			if src.Container.PasswordSecret != "" {
				pwSecret = src.Container.PasswordSecret
			}
			if member == "" {
				member = src.Container.Member
				memberIdx = src.Container.MemberIndex
			}
			if fmtHint == "" {
				fmtHint = src.Container.Format
			}
		} else if member == "" && src.Release.FileHint != "" {
			member = src.Release.FileHint
		}
		cacheSeed := ms.ReleaseID() + "|" + ms.Name()
		return openContainerOverVolumes(ctx, vols, []io.Closer{ms}, cacheSeed, fmtHint, member, memberIdx, src.Release.Track, pwSecret, effMIME, src.MaxBytes, digestsOf(src), deps)
	}

	// A deflate-compressed member streamed out of a provider's server-side ZIP
	// (such as TorBox's 100+-file auto-zip) is inflated into deps.Cache first,
	// then continues through normal container unwrapping (member-inside-a-member)
	// and digest verification (#709).
	if czs, ok := ms.(core.CompressedZipMemberSource); ok {
		decoded, err := extractCompressedZipMember(ctx, ms, czs, effMIME, src.MaxBytes, deps)
		_ = ms.Close()
		if err != nil {
			return nil, err
		}
		ms = decoded
	}

	wrapped := &mediaSourceByteSource{
		ms:   ms,
		name: path.Base(ms.Name()),
		mime: effMIME,
	}
	if wrapped.name == "" || wrapped.name == "." || wrapped.name == "/" {
		wrapped.name = ms.ReleaseID()
	}

	resolvedMember := false
	if rms, ok := ms.(core.ResolvedMemberSource); ok && rms.ResolvedArchiveMember() {
		resolvedMember = true
	}

	// Check if the release should compose with Container extraction (spec §4.3:
	// "If the resolved file in the release is itself an archive (.rar, .cbz, .7z),
	// release and container compose"). Use PrefetchReaderAt when available so a
	// backing-off Usenet candidate fails fast here instead of holding candidate
	// iteration for 45s.
	var head [probeHeadBytes]byte
	headLen := min(ms.Size(), int64(probeHeadBytes))
	var headSlice []byte
	if headLen > 0 {
		var (
			n    int
			rerr error
		)
		if pr, ok := ms.(core.PrefetchReaderAt); ok {
			n, rerr = pr.ReadAtPrefetchContext(ctx, head[:headLen], 0)
		} else if cr, ok := ms.(core.ContextReaderAt); ok {
			n, rerr = cr.ReadAtContext(ctx, head[:headLen], 0)
		} else {
			n, rerr = ms.ReadAt(head[:headLen], 0)
		}
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			_ = ms.Close()
			return nil, rerr
		}
		headSlice = head[:n]
	}

	archFmt := archive.Detect(headSlice)
	if !resolvedMember && (src.Container != nil || (archFmt != archive.FormatUnknown && shouldAutoComposeReleaseArchive(src, effMIME, wrapped.name, headSlice))) {
		pwSecret := src.Release.PasswordSecret
		var member string
		var memberIdx *int
		var fmtHint string
		if src.Container != nil {
			if src.Container.PasswordSecret != "" {
				pwSecret = src.Container.PasswordSecret
			}
			member = src.Container.Member
			memberIdx = src.Container.MemberIndex
			fmtHint = src.Container.Format
		} else if src.Release.FileHint != "" && !fileHintMatchesFile(src.Release.FileHint, wrapped.name) {
			member = src.Release.FileHint
		}
		cacheSeed := ms.ReleaseID() + "|" + wrapped.name
		return openContainerOverVolumes(ctx, []archive.Volume{{R: ms, Size: ms.Size()}}, []io.Closer{ms}, cacheSeed, fmtHint, member, memberIdx, src.Release.Track, pwSecret, effMIME, src.MaxBytes, digestsOf(src), deps)
	}

	declaredMIME := effMIME
	if declaredMIME == "" && !hasVideoFileExtension(wrapped.name) {
		// Non-video release files without an explicit plugin MIME must never
		// receive a guessed video/* MIME (#574).
		declaredMIME = "application/octet-stream"
	}
	resolvedMIME, merr := verifyAndResolveMIME(headSlice, declaredMIME, wrapped.name)
	if merr != nil {
		_ = ms.Close()
		return nil, merr
	}
	wrapped.mime = resolvedMIME
	keySum := sha256.Sum256([]byte(ms.ReleaseID() + "|" + wrapped.name))
	cacheKey := "rel-" + hex.EncodeToString(keySum[:12]) + "-" + wrapped.name
	return applyDigests(ctx, wrapped, digestsOf(src), src.MaxBytes, cacheKey, deps)
}

func fileHintMatchesFile(fileHint, fileName string) bool {
	hint := strings.TrimSpace(strings.ReplaceAll(fileHint, "\\", "/"))
	if hint == "" || fileName == "" {
		return false
	}
	return strings.EqualFold(path.Base(fileName), path.Base(hint))
}

func shouldAutoComposeReleaseArchive(src core.PlayableSource, effectiveMIME, fileName string, head []byte) bool {
	decl := normalizeMIME(effectiveMIME)
	if nzb.IsArchiveMIME(decl) {
		return false
	}
	switch strings.ToLower(path.Ext(fileName)) {
	case ".epub", ".cbz", ".cbr", ".cb7":
		return false
	}
	if magic, ok := sniffMagicBytes(head); ok && magic == "application/epub+zip" {
		return false
	}
	if nzb.MIMEFamily(decl) == "comic" {
		return false
	}
	if src.Release != nil && ((strings.TrimSpace(src.Release.FileHint) != "" && !fileHintMatchesFile(src.Release.FileHint, fileName)) || (src.Release.Track != nil && (src.Release.Track.Number > 0 || strings.TrimSpace(src.Release.Track.Title) != ""))) {
		return true
	}
	if decl == "application/octet-stream" {
		return false
	}
	// When a plugin explicitly selects a file in the release without a
	// ContainerRef or inner media MIME, serve the selected file itself rather
	// than unwrapping its first archive entry.
	if decl == "" && src.Release != nil && (hasExplicitReleaseSelector(*src.Release) || fileHintMatchesFile(src.Release.FileHint, fileName)) {
		return false
	}
	return true
}

func hasExplicitReleaseSelector(h core.ReleaseHint) bool {
	if strings.TrimSpace(h.File) != "" || h.FileIndex != nil {
		return true
	}
	return h.Candidate != nil && h.Candidate.FileIndex != nil
}

func hasExplicitReleaseOrMemberSelector(src core.PlayableSource) bool {
	if src.Release != nil && hasExplicitReleaseSelector(*src.Release) {
		return true
	}
	return src.Container != nil && (strings.TrimSpace(src.Container.Member) != "" || src.Container.MemberIndex != nil)
}

func hasVideoFileExtension(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".mkv", ".mp4", ".m4v", ".avi", ".mov", ".webm", ".ts", ".m2ts", ".mpg", ".mpeg", ".wmv", ".flv":
		return true
	default:
		return false
	}
}

func openContainerOverVolumes(
	ctx context.Context,
	vols []archive.Volume,
	closers []io.Closer,
	cacheSeed string,
	formatHint string,
	memberName string,
	memberIdx *int,
	track *core.TrackHint,
	passwordSecret string,
	declaredMIME string,
	maxBytes int64,
	digests expectedDigests,
	deps Deps,
) (ByteSource, error) {
	closeAll := func() {
		for _, cl := range closers {
			_ = cl.Close()
		}
	}
	var password string
	if strings.TrimSpace(passwordSecret) != "" {
		if deps.Secret == nil {
			closeAll()
			return nil, fmt.Errorf("kindstream: release archive requires password secret %q", passwordSecret)
		}
		pw, err := deps.Secret(ctx, strings.TrimSpace(passwordSecret))
		if err != nil {
			closeAll()
			return nil, err
		}
		password = pw
	}
	bufVols := make([]archive.Volume, len(vols))
	for i, v := range vols {
		bufVols[i] = archive.Volume{
			R:    newSeqBufferedReaderAt(ctx, v.R, v.Size),
			Size: v.Size,
		}
	}
	format, err := detectContainerFormat(bufVols[0], formatHint)
	if err != nil {
		closeAll()
		return nil, wrapArchiveUnstreamable(err)
	}
	members, zipEntries, err := listContainerMembers(ctx, format, bufVols, password)
	if err != nil {
		closeAll()
		return nil, wrapArchiveUnstreamable(err)
	}
	selected, nonDirIdx, err := selectArchiveMemberForMIME(members, memberName, memberIdx, track, declaredMIME)
	if err != nil {
		closeAll()
		return nil, err
	}
	// The archive directory already knows the member's size: a declared
	// size that disagrees rejects the wrong member before any data reads.
	if err := digests.checkSize(selected.Size); err != nil {
		closeAll()
		return nil, err
	}
	streamErr := selected.Streamable()
	if streamErr == nil {
		if format == archive.FormatZIP && len(selected.Extents) == 0 && selected.Size > 0 {
			resolved, rerr := resolveZIPMemberAt(bufVols, zipEntries, nonDirIdx)
			if rerr != nil {
				closeAll()
				return nil, wrapArchiveUnstreamable(rerr)
			}
			selected = resolved
		}
		mr, err := archive.NewMemberReader(selected, vols)
		if err != nil {
			closeAll()
			return nil, wrapArchiveUnstreamable(err)
		}
		var head [probeHeadBytes]byte
		headLen := min(selected.Size, int64(probeHeadBytes))
		var headSlice []byte
		if headLen > 0 {
			n, _ := mr.ReadAt(head[:headLen], 0)
			headSlice = head[:n]
		}
		resolvedMIME, merr := verifyAndResolveMIME(headSlice, declaredMIME, selected.Name)
		if merr != nil {
			closeAll()
			return nil, merr
		}
		memberSrc := &containerMemberSource{
			r:       mr,
			size:    selected.Size,
			name:    memberDisplayName(selected.Name),
			mime:    resolvedMIME,
			closers: closers,
		}
		keySum := sha256.Sum256([]byte(cacheSeed + "|" + selected.Name))
		memberKey := "relmember-" + hex.EncodeToString(keySum[:12]) + "-" + memberSrc.name
		return applyDigests(ctx, memberSrc, digests, maxBytes, memberKey, deps)
	}
	if errors.Is(streamErr, archive.ErrCompressed) || errors.Is(streamErr, archive.ErrSolid) {
		defer closeAll()
		return extractFromOpenVolumes(ctx, vols, cacheSeed, format, memberName, selected, nonDirIdx, declaredMIME, maxBytes, digests, deps)
	}
	closeAll()
	return nil, wrapArchiveUnstreamable(streamErr)
}

// ---------------------------------------------------------------------------
// MIME sniffing and verification (spec §4.3, §5.1)

func finalizeSourceMIME(src ByteSource, declaredMIME, fallbackName string) (ByteSource, error) {
	headLen := min(src.Size(), int64(probeHeadBytes))
	var head []byte
	if headLen > 0 {
		head = make([]byte, headLen)
		n, err := src.ReadAt(head, 0)
		if err != nil && !errors.Is(err, io.EOF) {
			_ = src.Close()
			return nil, err
		}
		head = head[:n]
	}
	name := src.Name()
	if name == "" {
		name = fallbackName
	}
	resolvedMIME, err := verifyAndResolveMIME(head, declaredMIME, name)
	if err != nil {
		_ = src.Close()
		return nil, err
	}
	if resolvedMIME == src.MIME() && name == src.Name() {
		return src, nil
	}
	return &renamedByteSource{
		ByteSource: src,
		name:       name,
		mime:       resolvedMIME,
	}, nil
}

// verifyAndResolveMIME sniffs the first 512 bytes of a stream and compares the
// detected type against the plugin-declared MIME.
//
// If the upstream returned an HTML error page (text/html) when the plugin
// declared a non-HTML type, or if positive magic bytes belong to a different
// media family than declared (e.g. audio/* vs image/*), ErrMIMEMismatch is
// returned so OpenSources falls back to the next core.PlayableSource.
// When magic bytes identify a more specific MIME within the same family (e.g.
// image/png when declared was image/jpeg), the sniffed MIME wins (spec §5.1).
func verifyAndResolveMIME(head []byte, declared string, filename string) (string, error) {
	declaredBase := normalizeMIME(declared)
	magicMIME, positiveMagic := sniffMagicBytes(head)

	if positiveMagic {
		if magicMIME == "text/html" && declaredBase != "" && declaredBase != "text/html" {
			return "", fmt.Errorf("%w: declared %q, sniffed %q", ErrMIMEMismatch, declaredBase, magicMIME)
		}
		if declaredBase == "application/octet-stream" {
			return "application/octet-stream", nil
		}
		if declaredBase != "" {
			declFam := mimeFamily(declaredBase)
			sniffFam := mimeFamily(magicMIME)
			if declFam != "" && sniffFam != "" && declFam != sniffFam &&
				!compatibleArchiveMIME(declaredBase, magicMIME) &&
				!compatibleBookMIME(declaredBase, magicMIME) {
				return "", fmt.Errorf("%w: declared %q (%s), sniffed %q (%s)", ErrMIMEMismatch, declaredBase, declFam, magicMIME, sniffFam)
			}
			if declFam == "" {
				// Preserve explicit custom application/* or vendor MIME types.
				return declaredBase, nil
			}
		}
		// Keep comicbook/ebook subtype if magic is the underlying container (e.g. application/vnd.comicbook+zip over zip).
		if compatibleArchiveMIME(declaredBase, magicMIME) && declaredBase != "" {
			return declaredBase, nil
		}
		if declaredBase == "" && strings.HasPrefix(magicMIME, "video/") && filename != "" && !hasVideoFileExtension(filename) {
			return "application/octet-stream", nil
		}
		return magicMIME, nil
	}

	if declaredBase != "" {
		return declaredBase, nil
	}
	if len(head) > 0 {
		detected := normalizeMIME(http.DetectContentType(head))
		if detected != "" && !strings.HasPrefix(detected, "video/") {
			return detected, nil
		}
	}
	return "application/octet-stream", nil
}

// sniffMagicBytes checks well-known magic byte signatures for images, audio,
// video, ebooks, comic archives, and HTML error pages (spec §4.3).
func sniffMagicBytes(head []byte) (mime string, positive bool) {
	if len(head) == 0 {
		return "", false
	}
	switch {
	case len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF:
		return "image/jpeg", true
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png", true
	case bytes.HasPrefix(head, []byte("GIF87a")) || bytes.HasPrefix(head, []byte("GIF89a")):
		return "image/gif", true
	case len(head) >= 12 && bytes.Equal(head[0:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return "image/webp", true
	case bytes.HasPrefix(head, []byte("%PDF-")):
		return "application/pdf", true
	case bytes.HasPrefix(head, []byte("fLaC")):
		return "audio/flac", true
	case bytes.HasPrefix(head, []byte("ID3")) || (len(head) >= 2 && head[0] == 0xFF && (head[1]&0xE0) == 0xE0 && (head[1]&0x18) != 0x08 && (head[1]&0x06) != 0x00):
		return "audio/mpeg", true
	case bytes.HasPrefix(head, []byte("OggS")):
		if bytes.Contains(head[:min(len(head), 64)], []byte("OpusHead")) {
			return "audio/opus", true
		}
		return "audio/ogg", true
	case len(head) >= 12 && bytes.Equal(head[0:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WAVE")):
		return "audio/wav", true
	case bytes.HasPrefix(head, []byte("\x1A\x45\xDF\xA3")):
		if bytes.Contains(head[:min(len(head), 64)], []byte("webm")) {
			return "video/webm", true
		}
		return "video/x-matroska", true
	case len(head) >= 12 && bytes.Equal(head[4:8], []byte("ftyp")):
		brand := string(head[8:12])
		switch brand {
		case "avif", "avis":
			return "image/avif", true
		case "M4A ", "M4B ", "F4A ":
			return "audio/mp4", true
		default:
			return "video/mp4", true
		}
	case bytes.HasPrefix(head, []byte("PK\x03\x04")), bytes.HasPrefix(head, []byte("PK\x05\x06")), bytes.HasPrefix(head, []byte("PK\x07\x08")):
		if len(head) >= 58 && bytes.Equal(head[30:58], []byte("mimetypeapplication/epub+zip")) {
			return "application/epub+zip", true
		}
		return "application/zip", true
	case bytes.HasPrefix(head, []byte("Rar!\x1a\x07")):
		return "application/vnd.rar", true
	case bytes.HasPrefix(head, []byte("7z\xbc\xaf\x27\x1c")):
		return "application/x-7z-compressed", true
	}

	trimmed := bytes.TrimLeft(head, " \t\r\n")
	lower := bytes.ToLower(trimmed[:min(len(trimmed), 64)])
	if bytes.HasPrefix(lower, []byte("<!doctype html")) ||
		bytes.HasPrefix(lower, []byte("<html")) ||
		bytes.HasPrefix(lower, []byte("<head")) ||
		bytes.HasPrefix(lower, []byte("<body")) {
		return "text/html", true
	}
	return "", false
}

func normalizeMIME(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if semi := strings.IndexByte(m, ';'); semi >= 0 {
		m = strings.TrimSpace(m[:semi])
	}
	return m
}

func mimeFamily(m string) string {
	m = normalizeMIME(m)
	switch {
	case strings.HasPrefix(m, "image/"):
		return "image"
	case strings.HasPrefix(m, "audio/"):
		return "audio"
	case strings.HasPrefix(m, "video/"):
		return "video"
	case strings.HasPrefix(m, "text/"):
		return "text"
	case m == "application/pdf":
		return "pdf"
	case isDeclaredArchiveMIME(m):
		return "archive"
	default:
		return ""
	}
}

func isDeclaredArchiveMIME(m string) bool {
	switch normalizeMIME(m) {
	case "application/zip",
		"application/x-zip-compressed",
		"application/vnd.comicbook+zip",
		"application/epub+zip",
		"application/vnd.rar",
		"application/x-rar-compressed",
		"application/vnd.comicbook-rar",
		"application/x-cbr",
		"application/x-cbz",
		"application/x-7z-compressed",
		"application/vnd.comicbook+7z":
		return true
	default:
		return false
	}
}

func compatibleArchiveMIME(declared, sniffed string) bool {
	if isDeclaredArchiveMIME(declared) && isDeclaredArchiveMIME(sniffed) {
		return true
	}
	// Audio MP4 / M4B containers often sniff with a generic MP4 ftyp brand.
	if (strings.HasPrefix(declared, "audio/") && sniffed == "video/mp4") ||
		(strings.HasPrefix(declared, "video/") && sniffed == "audio/mp4") {
		return true
	}
	return false
}

func compatibleBookMIME(declared, sniffed string) bool {
	return nzb.MIMEFamily(declared) == "book" && nzb.MIMEFamily(sniffed) == "book"
}

// ---------------------------------------------------------------------------
// Transcode bridge & HTTP serving helpers for Slice J (spec §15.1)

// Seeker wraps any ByteSource as an io.ReadSeekCloser so it can be passed to
// http.ServeContent, standard library readers, or transcode pipelines.
func Seeker(src ByteSource) io.ReadSeekCloser {
	if rsc, ok := src.(io.ReadSeekCloser); ok {
		return rsc
	}
	return &byteSourceSeeker{
		src:  src,
		sr:   io.NewSectionReader(src, 0, src.Size()),
		size: src.Size(),
	}
}

type byteSourceSeeker struct {
	src  ByteSource
	sr   *io.SectionReader
	size int64
}

func (b *byteSourceSeeker) Read(p []byte) (int, error) {
	return b.sr.Read(p)
}

func (b *byteSourceSeeker) Seek(offset int64, whence int) (int64, error) {
	return b.sr.Seek(offset, whence)
}

func (b *byteSourceSeeker) Close() error {
	return b.src.Close()
}

// Handler returns an http.Handler that serves a ByteSource with full HTTP Range
// support (206 Partial Content, Accept-Ranges: bytes, Content-Type, and
// Content-Length).
func Handler(src ByteSource) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := strings.TrimSpace(src.MIME())
		if m == "" {
			m = "application/octet-stream"
		}
		w.Header().Set("Content-Type", m)
		sr := io.NewSectionReader(ReadAhead(r.Context(), src, src.Size()), 0, src.Size())
		http.ServeContent(w, r, src.Name(), time.Time{}, sr)
	})
}

// ServeLoopback starts a loopback HTTP server (http://127.0.0.1:<port>/<name>)
// backed by src so local ffmpeg / ffprobe sessions in internal/transcode can
// seek and stream any ByteSource over HTTP Range (spec §15.1).
// Calling closeFn shuts down the loopback server without closing src.
func ServeLoopback(src ByteSource) (urlStr string, closeFn func() error, err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("kindstream: listen loopback: %w", err)
	}
	srv := &http.Server{
		Handler:           Handler(src),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		_ = srv.Serve(ln)
	}()
	name := path.Clean("/" + src.Name())
	if name == "/" || name == "/." {
		name = "/stream"
	}
	var once sync.Once
	closeServer := func() error {
		var cerr error
		once.Do(func() {
			cerr = srv.Close()
		})
		return cerr
	}
	return "http://" + ln.Addr().String() + name, closeServer, nil
}

// MediaSource adapts a ByteSource into a core.MediaSource so any pipeline
// accepting core.MediaSource can consume a kindstream ByteSource directly.
func MediaSource(src ByteSource, releaseID string) core.MediaSource {
	if releaseID == "" {
		releaseID = src.Name()
	}
	return &byteSourceMediaAdapter{
		ByteSource: src,
		releaseID:  releaseID,
	}
}

type byteSourceMediaAdapter struct {
	ByteSource
	releaseID string
}

func (a *byteSourceMediaAdapter) ReleaseID() string { return a.releaseID }

func (a *byteSourceMediaAdapter) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if cr, ok := a.ByteSource.(core.ContextReaderAt); ok {
		return cr.ReadAtContext(ctx, p, off)
	}
	return a.ByteSource.ReadAt(p, off)
}

// UnwrapMediaSource returns the underlying core.MediaSource when bs wraps one
// directly without archive extraction, preserving optional playback interfaces.
func UnwrapMediaSource(bs ByteSource) core.MediaSource {
	for bs != nil {
		switch v := bs.(type) {
		case *mediaSourceByteSource:
			return v.ms
		case *renamedByteSource:
			bs = v.ByteSource
		case interface{ UnwrapByteSource() ByteSource }:
			bs = v.UnwrapByteSource()
		default:
			return nil
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Internal helpers

func effectiveDownloadMaxBytes(srcMax int64, deps Deps) int64 {
	var grant int64
	if deps.CacheWriteLimits != nil {
		grant, _ = deps.CacheWriteLimits()
	}
	return effectiveCacheWriteLimit(srcMax, grant, deps)
}

func effectiveMemberMaxBytes(srcMax int64, deps Deps) int64 {
	var grant int64
	if deps.CacheWriteLimits != nil {
		_, grant = deps.CacheWriteLimits()
	}
	return effectiveCacheWriteLimit(srcMax, grant, deps)
}

func effectiveCacheWriteLimit(srcMax, grant int64, deps Deps) int64 {
	// Bound the in-memory fallback when no disk namespace is available.
	const maxInMemoryBytes int64 = 2 << 30
	limit := maxInMemoryBytes
	if deps.Cache != nil {
		limit = deps.Cache.Quota().EffectiveMaxBytes()
	}
	for _, cap := range []int64{grant, deps.MaxDownloadBytes, srcMax} {
		if cap > 0 {
			limit = min(limit, cap)
		}
	}
	return limit
}

func downloadCacheKey(rawURL string, itemIdx int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s#%d", rawURL, itemIdx)))
	return "dl-" + hex.EncodeToString(h[:12]) + "-" + defaultNameFromURL(rawURL)
}

type limitEnforcingReader struct {
	r    io.Reader
	read int64
	max  int64
}

func (l *limitEnforcingReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if n > 0 {
		l.read += int64(n)
		if l.read > l.max {
			return n, fmt.Errorf("%w: stream exceeded %d bytes", ErrSizeLimitExceeded, l.max)
		}
	}
	return n, err
}

func readIntoMemoryByteSource(r io.Reader, limit, expectedSize int64, digests expectedDigests, name, declaredMIME string) (ByteSource, error) {
	if limit > 0 {
		r = &limitEnforcingReader{r: r, max: limit}
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if expectedSize > 0 && int64(len(data)) != expectedSize {
		return nil, fmt.Errorf("kindstream: size mismatch: got %d bytes, want %d", len(data), expectedSize)
	}
	if digests.size <= 0 {
		digests.size = expectedSize
	}
	if err := digests.verifyBytes(data); err != nil {
		return nil, err
	}
	head := data[:min(len(data), probeHeadBytes)]
	resolvedMIME, err := verifyAndResolveMIME(head, declaredMIME, name)
	if err != nil {
		return nil, err
	}
	return &bytesByteSource{
		r:    bytes.NewReader(data),
		size: int64(len(data)),
		name: name,
		mime: resolvedMIME,
	}, nil
}

type bytesByteSource struct {
	r    *bytes.Reader
	size int64
	name string
	mime string
}

func (b *bytesByteSource) ReadAt(p []byte, off int64) (int, error) {
	return b.r.ReadAt(p, off)
}

func (b *bytesByteSource) Size() int64  { return b.size }
func (b *bytesByteSource) Name() string { return b.name }
func (b *bytesByteSource) MIME() string { return b.mime }
func (b *bytesByteSource) Close() error { return nil }

type renamedByteSource struct {
	ByteSource
	name string
	mime string
}

func (r *renamedByteSource) Name() string {
	if r.name != "" {
		return r.name
	}
	return r.ByteSource.Name()
}

func (r *renamedByteSource) MIME() string {
	if r.mime != "" {
		return r.mime
	}
	return r.ByteSource.MIME()
}

func (r *renamedByteSource) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if cr, ok := r.ByteSource.(core.ContextReaderAt); ok {
		return cr.ReadAtContext(ctx, p, off)
	}
	return r.ByteSource.ReadAt(p, off)
}

type mediaSourceByteSource struct {
	ms   core.MediaSource
	name string
	mime string
}

func (m *mediaSourceByteSource) ReadAt(p []byte, off int64) (int, error) {
	return m.ms.ReadAt(p, off)
}

func (m *mediaSourceByteSource) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if cr, ok := m.ms.(core.ContextReaderAt); ok {
		return cr.ReadAtContext(ctx, p, off)
	}
	return m.ms.ReadAt(p, off)
}

func (m *mediaSourceByteSource) Size() int64  { return m.ms.Size() }
func (m *mediaSourceByteSource) Name() string { return m.name }
func (m *mediaSourceByteSource) MIME() string { return m.mime }
func (m *mediaSourceByteSource) Close() error { return m.ms.Close() }
