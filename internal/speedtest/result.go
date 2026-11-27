package speedtest

import "time"

// Duration caps the measurement window, including opening its connections.
const Duration = 10 * time.Second

// Result contains measurements only. It must never contain upstream error
// text, credentials, URLs, release titles or article identifiers.
type Result struct {
	Status            string    `json:"status"`
	At                time.Time `json:"at"`
	Seconds           float64   `json:"seconds"`
	Bytes             int64     `json:"bytes"`
	MBps              float64   `json:"mbps"`
	MBpsPerConnection float64   `json:"mbpsPerConnection"`
	StreamMbps        float64   `json:"streamMbps"`
	TypicalMbps       float64   `json:"typicalMbps,omitempty"`
	Connections       int       `json:"connections"`
	Opened            int       `json:"opened"`
	Refused           int       `json:"refused"`
	Misses            int       `json:"misses"`
	StreamsPlaying    int       `json:"streamsPlaying,omitempty"`
	SpareOnly         bool      `json:"spareOnly"`
}

func (r *Result) Measure(start time.Time) {
	r.MeasureTransfer(time.Since(start))
}

// MeasureTransfer records a rate over the supplied transfer interval.
func (r *Result) MeasureTransfer(elapsed time.Duration) {
	r.Seconds = elapsed.Seconds()
	if r.Bytes <= 0 || r.Connections <= 0 || r.Seconds <= 0 {
		return
	}
	r.MBps = float64(r.Bytes) / (1e6 * r.Seconds)
	r.MBpsPerConnection = r.MBps / float64(r.Connections)
	// Leave 20% headroom for variable bitrate, protocol overhead and bursts.
	r.StreamMbps = r.MBps * 8 * 0.8
	r.Status = "ok"
}

// Observation is a playback sample with no viewer or release identity.
type Observation struct {
	At            time.Time `json:"at"`
	SourceReadBps int64     `json:"sourceReadBps"`
	MediaBps      int64     `json:"mediaBps"`
}

// ValidStatus restricts wire results to application-defined outcomes.
func (r Result) ValidStatus() bool {
	switch r.Status {
	case "ok", "unavailable", "paused", "busy", "empty_vault", "no_readable_nzb", "sample_missing", "no_recent_file", "already_running":
		return true
	}
	return false
}
