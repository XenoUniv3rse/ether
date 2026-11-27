package lineup

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

const (
	BaseMarker = "\x00ether-origin"
	UserMarker = "\x00ether-user"
	PassMarker = "\x00ether-password"
)

var m3uEscaper = strings.NewReplacer("\r", " ", "\n", " ", "\"", "'")

func WriteLive(out io.Writer, c frontend.LiveChannel, image, origin, user, pass string, id int32, ext string) error {
	group := strings.TrimSpace(c.Group)
	if len(c.Groups) > 0 {
		group = strings.TrimSpace(c.Groups[0].Name)
	}
	if group == "" {
		group = "Live TV"
	}
	_, err := fmt.Fprintf(out, "#EXTINF:-1 tvg-id=\"%s\" tvg-logo=\"%s\" group-title=\"%s\",%s\n%s/live/%s/%s/%d.%s\n", m3uEscaper.Replace(c.ID), m3uEscaper.Replace(image), m3uEscaper.Replace(group), m3uEscaper.Replace(c.Name), origin, user, pass, id, ext)
	return err
}

// WriteVOD writes the same entries for a direct export and a snapshot build.
func WriteVOD(ctx context.Context, out io.Writer, origin, user, pass string, batchSize int,
	walk func(string, func(context.Context, frontend.Title, int32) error) error,
	children func(context.Context, frontend.RatingKey, func(frontend.Title) error) error,
	compact func(context.Context, []string) (map[string]int32, error),
) error {
	if err := walk("movie", func(_ context.Context, title frontend.Title, id int32) error {
		_, err := fmt.Fprintf(out, "#EXTINF:-1 group-title=\"Movies\",%s\n%s/movie/%s/%s/%d.mkv\n", m3uEscaper.Replace(title.Title), origin, user, pass, id)
		return err
	}); err != nil {
		return err
	}
	type episodeLine struct {
		show, title     string
		season, episode int
	}
	return frontend.WalkCompactTitles(ctx, batchSize, compact, func(emit func(frontend.RatingKey, int32, episodeLine) error) error {
		return walk("show", func(ctx context.Context, show frontend.Title, _ int32) error {
			return children(ctx, show.Key, func(season frontend.Title) error {
				if season.Type != frontend.TypeSeason {
					return nil
				}
				return children(ctx, season.Key, func(ep frontend.Title) error {
					if ep.Type != frontend.TypeEpisode {
						return nil
					}
					return emit(ep.Key, frontend.ExportCompactID(ctx, ep.Key), episodeLine{show.Title, ep.Title, ep.Season, ep.Episode})
				})
			})
		})
	}, func(item episodeLine, id int32) error {
		_, err := fmt.Fprintf(out, "#EXTINF:-1 group-title=\"Series\",%s S%02dE%02d %s\n%s/series/%s/%s/%d.mkv\n", m3uEscaper.Replace(item.show), item.season, item.episode, m3uEscaper.Replace(item.title), origin, user, pass, id)
		return err
	})
}

// WriteSnapshot substitutes only URL lines. Media titles can contain the
// marker bytes without expanding them or exposing credentials.
func WriteSnapshot(out io.Writer, body io.Reader, origin, user, pass string) error {
	in := bufio.NewReaderSize(body, 64<<10)
	replace := strings.NewReplacer(BaseMarker, origin, UserMarker, user, PassMarker, pass)
	urlLine := false
	for {
		line, err := in.ReadString('\n')
		if len(line) != 0 {
			if urlLine {
				if _, werr := replace.WriteString(out, line); werr != nil {
					return werr
				}
			} else if _, werr := io.WriteString(out, line); werr != nil {
				return werr
			}
			urlLine = !urlLine
		}
		if err != nil {
			if err == io.EOF && len(line) == 0 && !urlLine {
				return nil
			}
			return err
		}
	}
}
