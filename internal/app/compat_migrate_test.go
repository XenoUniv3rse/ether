package app

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestSplitAppOverrides(t *testing.T) {
	in := []core.ClientCapabilityOverride{{Product: "Plex for Android", MaxHeight: 2160}}
	got, count := splitAppOverrides(in)
	if count != 1 || len(got) != 3 || got[0].Product != "Plex for Android" || got[1].Product != "Plex for Android (Mobile)" || got[2].Product != "Plex for Android (TV)" || got[0].MaxHeight != 2160 || got[1].MaxHeight != 2160 || got[2].MaxHeight != 2160 {
		t.Fatalf("count=%d got=%+v", count, got)
	}
	_, count = splitAppOverrides(got)
	if count != 0 {
		t.Fatal("migration must be idempotent")
	}
}
