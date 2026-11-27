package frontend

import (
	"context"
	"io"

	"github.com/braedonsaunders/ether/internal/core"
)

// OfflineDownloadSource owns one opened, immutable byte representation.
type OfflineDownloadSource struct {
	io.ReaderAt
	io.Closer
	Release, Name, MIME, ETag string
	Size                      int64
	Media                     MediaInfo // Probe facts from the original bytes served by ReaderAt.
	ReleaseName               string    // Internal subtitle cache lookup, never sent to the client.
}

// OfflineDownloadSidecars is optionally implemented by Host.Subtitles. It
// lists the title's already cached sidecars for the opened release.
type OfflineDownloadSidecars interface {
	CachedDownloadSidecars(context.Context, *Title, *Viewer, *OfflineDownloadSource) []StreamInfo
}

type OfflineDownloadSources interface {
	OpenOfflineDownload(context.Context, *Viewer, *Title, ClientCaps, string, *OriginalPlaybackCapabilities) (*OfflineDownloadSource, error)
}

// OfflineDownloadOpener opens an original without admitting a playback session.
type OfflineDownloadOpener interface {
	OpenOriginalDownload(context.Context, *Viewer, *Title, ClientCaps, string, *core.KindInput, *OriginalPlaybackCapabilities) (*OfflineDownloadSource, error)
}

type OfflineDownloadBinding = core.OfflineDownloadBinding

// OfflineDownloadQuota binds an idempotency receipt and accounts for the
// original in the same transaction as the shared download quota.
type OfflineDownloadQuota interface {
	OfflineDownloadReceipt(context.Context, *Viewer, string) (OfflineDownloadBinding, error)
	AdmitOfflineDownload(context.Context, *Viewer, RatingKey, string, OfflineDownloadBinding, int64) (DownloadLimitDecision, error)
	RemainingDailyDownloads(context.Context, *Viewer) (limit, remaining int, ok bool)
}
