package frontend

import (
	"testing"
	"time"
)

func TestCatchUpEpisodeTitle(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	local := time.Local
	time.Local = ny
	t.Cleanup(func() { time.Local = local })

	start := time.Date(2026, 11, 25, 23, 30, 0, 0, time.UTC) // 6:30 PM Eastern
	news := LiveTVProgramme{Start: start, Item: LiveTVItem{Title: "World News Tonight"}}
	if got := CatchUpEpisodeTitle(news); got != "Wed Nov 25, 6:30 PM" {
		t.Fatalf("untitled airing: %q", got)
	}
	reair := news
	reair.Start = start.Add(5 * time.Hour) // 11:30 PM the same evening
	if got := CatchUpEpisodeTitle(reair); got == CatchUpEpisodeTitle(news) || got != "Wed Nov 25, 11:30 PM" {
		t.Fatalf("re-airing is not told apart: %q", got)
	}
	named := news
	named.Item.EpisodeTitle = "The Pilot"
	if got := CatchUpEpisodeTitle(named); got != "The Pilot" {
		t.Fatalf("episode title: %q", got)
	}
}
