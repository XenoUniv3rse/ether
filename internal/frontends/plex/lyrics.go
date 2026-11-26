package plex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// lyricsProvider names Ether as the lyric stream's source.
const lyricsProvider = "tv.ether.lyrics"

// addTrackLyrics gives the lyric stream the same client-safe ID shape as
// other Plex streams. The binding carries identity only.
func (s *Server) addTrackLyrics(ctx context.Context, viewer *frontend.Viewer, media []model.Media, key frontend.RatingKey, lyrics frontend.LyricAvailability) {
	if lyrics == frontend.LyricsAbsent {
		return
	}
	for _, version := range media {
		for _, part := range version.Part {
			if len(part.Stream) > 0 {
				id := s.assignStream(ctx, viewer, frontend.StreamBinding{Key: key, Index: -1, Info: frontend.StreamInfo{Type: "lyrics"}})
				if id != 0 {
					addLyricStream(media, id, lyrics)
				}
				return
			}
		}
	}
}

// addLyricStream lists a lyric stream (streamType 4) on parts that list streams.
func addLyricStream(media []model.Media, id int64, lyrics frontend.LyricAvailability) {
	format, timed := "lrc", lyrics != frontend.LyricsPlain
	if !timed {
		format = "txt"
	}
	for m := range media {
		for p := range media[m].Part {
			part := &media[m].Part[p]
			if len(part.Stream) == 0 {
				continue
			}
			part.Stream = append(part.Stream, model.Stream{
				ID:         id,
				StreamType: 4,
				Codec:      format,
				Format:     format,
				Key:        fmt.Sprintf("/library/streams/%d", id),
				Timed:      model.Ptr(timed),
				Provider:   lyricsProvider,
			})
		}
	}
}

// serveLyricStream answers /library/streams/{id} for a lyric stream: the
// track's lyrics as LRC (synced lines with [mm:ss.xx] stamps), or plain text
// when the source has no timing.
func (s *Server) serveLyricStream(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, key frontend.RatingKey) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	source, ok := s.exp().(frontend.TitleLyrics)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	lyrics, err := source.TitleLyrics(r.Context(), viewer, key)
	if errors.Is(err, frontend.ErrNotFound) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	}
	body := lyricsText(lyrics)
	if body == "" {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(body))
}

// lyricsText renders lyrics as simple LRC, the only form Plexamp reads:
// one [mm:ss.xx] stamp per line, no word timing. Plain lyrics keep their
// lines unstamped.
func lyricsText(l frontend.Lyrics) string {
	var b strings.Builder
	if len(l.Lines) > 0 {
		for _, line := range l.Lines {
			cs := line.Start.Milliseconds() / 10
			fmt.Fprintf(&b, "[%02d:%02d.%02d]%s\n", cs/6000, (cs/100)%60, cs%100, line.Text)
		}
		return b.String()
	}
	plain := strings.TrimSpace(strings.ReplaceAll(l.Plain, "\r\n", "\n"))
	if plain != "" {
		return plain + "\n"
	}
	if l.Instrumental {
		return "♪\n"
	}
	return ""
}
