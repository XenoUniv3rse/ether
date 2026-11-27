package app

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/subtitles"
)

func TestSharedSubtitleWriteRequiresOwner(t *testing.T) {
	svc := subtitles.New(subtitles.Options{CacheDir: t.TempDir()})
	surface := subtitleSurface{app: &App{subtitles: svc}}
	ctx := context.Background()
	title := &core.Title{Key: core.MovieKey(42)}
	owner := &core.User{IsOwner: true}
	body := []byte("1\n00:00:01,000 --> 00:00:02,000\nHello\n\n")
	track, err := surface.UploadSubtitle(ctx, title, owner, "movie.en.srt", "srt", "en", body)
	if err != nil {
		t.Fatal(err)
	}
	for _, viewer := range []*core.User{nil, {}} {
		if err := surface.DeleteSubtitle(ctx, title.Key, viewer, track); !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("shared deletion returned %v", err)
		}
		if _, err := surface.UploadSubtitle(ctx, title, viewer, "movie.en.srt", "srt", "en", body); !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("shared replacement returned %v", err)
		}
	}
	if err := surface.DeleteSubtitle(ctx, title.Key, owner, track); err != nil {
		t.Fatal(err)
	}
}
