package stats

import (
	"context"
	"sort"
	"time"
)

type HeatCell struct {
	Weekday   int   `json:"weekday"`
	Hour      int   `json:"hour"`
	Plays     int64 `json:"plays"`
	WatchedMS int64 `json:"watchedMs"`
}
type DailyDelivery struct {
	Label     string `json:"label"`
	Direct    int64  `json:"direct"`
	Transcode int64  `json:"transcode"`
	Bytes     int64  `json:"bytes"`
}
type activityEvent struct {
	at    int64
	delta int
}

func (s *Service) activitySummary(ctx context.Context, out *Summary, where string, args []any, shift int) error {
	rows, err := s.db.QueryContext(ctx, `SELECT started,ended,updated,watched_ms,bytes,decision,audio_copy`+where, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	heat := map[int]*HeatCell{}
	daily := map[string]*DailyDelivery{}
	events := []activityEvent{}
	for rows.Next() {
		var start, end, updated, watched, bytes int64
		var decision string
		var copyAudio *int
		if err = rows.Scan(&start, &end, &updated, &watched, &bytes, &decision, &copyAudio); err != nil {
			return err
		}
		at := time.Unix(start+int64(shift), 0).UTC()
		key := int(at.Weekday())*24 + at.Hour()
		if heat[key] == nil {
			heat[key] = &HeatCell{Weekday: int(at.Weekday()), Hour: at.Hour()}
		}
		heat[key].Plays++
		heat[key].WatchedMS += watched
		label := at.Format("2006-01-02")
		if daily[label] == nil {
			daily[label] = &DailyDelivery{Label: label}
		}
		d := daily[label]
		d.Bytes += bytes
		if decision == "directplay" || (decision == "copy" && copyAudio != nil && *copyAudio == 1) {
			d.Direct++
		} else if decision == "transcode" || (decision == "copy" && copyAudio != nil && *copyAudio == 0) {
			d.Transcode++
		}
		if end == 0 {
			end = updated
		}
		if end > start {
			events = append(events, activityEvent{start, 1}, activityEvent{end, -1})
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	out.Heatmap = []HeatCell{}
	for _, v := range heat {
		out.Heatmap = append(out.Heatmap, *v)
	}
	sort.Slice(out.Heatmap, func(i, j int) bool {
		a, b := out.Heatmap[i], out.Heatmap[j]
		return a.Weekday*24+a.Hour < b.Weekday*24+b.Hour
	})
	out.DailyDelivery = []DailyDelivery{}
	for _, v := range daily {
		out.DailyDelivery = append(out.DailyDelivery, *v)
	}
	sort.Slice(out.DailyDelivery, func(i, j int) bool { return out.DailyDelivery[i].Label < out.DailyDelivery[j].Label })
	sort.Slice(events, func(i, j int) bool {
		if events[i].at == events[j].at {
			return events[i].delta < events[j].delta
		}
		return events[i].at < events[j].at
	})
	active := 0
	for _, e := range events {
		active += e.delta
		if active > out.PeakStreams {
			out.PeakStreams = active
		}
	}
	return nil
}
