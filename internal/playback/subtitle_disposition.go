package playback

import (
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langtag"
)

// Correct a reversed pair only when a named forced track has one same-language,
// same-codec partner with the corresponding plain title and opposite disposition.
func correctReversedSubtitleDispositions(streams []core.StreamInfo) {
	for i := range streams {
		forced := &streams[i]
		if forced.Type != "subtitle" || forced.External || forced.Forced {
			continue
		}
		title := strings.ToLower(strings.TrimSpace(forced.Title))
		if !langtag.ForcedSubtitleTitle(title) {
			continue
		}
		plain := strings.TrimSpace(strings.TrimSuffix(title, "(forced)"))
		if plain == "" || plain == title {
			continue
		}
		match := -1
		for j, other := range streams {
			if other.Type != "subtitle" || other.External || other.Codec != forced.Codec ||
				!strings.EqualFold(strings.TrimSpace(other.Title), plain) ||
				langtag.Normalize(other.Language) != langtag.Normalize(forced.Language) {
				continue
			}
			if match != -1 {
				match = -1
				break
			}
			match = j
		}
		if match >= 0 && streams[match].Forced {
			full, partial := false, true
			streams[match].Forced, streams[match].ForcedDisposition = full, &full
			forced.Forced, forced.ForcedDisposition = partial, &partial
		}
	}
}
