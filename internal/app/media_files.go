package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/segcache"
)

func (a *App) openMediaFiles(ctx context.Context, disk segcache.DiskUsageFunc) error {
	dir := filepath.Join(a.cache.Directory(), "plugin-media")
	legacy := filepath.Join(a.opts.DataDir, "blobcache")
	if !a.cache.Shared() {
		// A same-volume rename adopts the complete store, including open-ended
		// resumable downloads and index metadata, without copying media.
		if src, err := os.Lstat(legacy); err == nil && src.IsDir() {
			if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
				_ = os.Rename(legacy, dir) // Cross-volume and existing stores use verified import below.
			}
		}
	}
	files, err := blobcache.Open(dir, blobcache.Options{Logger: a.log, DiskUsage: disk, Unified: true,
		Shared: a.cache.Shared(), CheckVolume: a.cache.CheckVolume,
		PoolCapacity: a.cache.MediaFileCapacity, MakeFileRoom: a.cache.MakeFileRoom})
	if err != nil {
		return err
	}
	a.mediaFiles = files
	a.cache.SetFilePool(files)
	a.closers = append(a.closers, func() { a.cache.SetFilePool(nil); _ = files.Close() })
	if err := files.ImportLegacy(ctx, legacy); err != nil {
		a.log.Warn("media cache: legacy import incomplete; retained source files will be retried on restart", "err", err)
	}
	return ctx.Err()
}
