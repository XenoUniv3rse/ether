package transcode

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// pceSeekInput builds a private, generated concat manifest. Its first short
// span primes the AAC decoder with the PCE; the second is a normal demuxer
// seek. Never accept a manifest from the source. The nested format is pinned
// to MPEG-TS so an untrusted source cannot introduce a playlist of its own.
func pceSeekInput(s runSpec) (uri string, prelude time.Duration, ok bool) {
	if s.Protocol != "hls" || s.Decision.Container == ContainerMatroska || s.Start <= 0 || s.Info.Container != "mpegts" || s.Decision.AudioCopy {
		return "", 0, false
	}
	a := findStream(s.Info, s.Decision.AudioStream, "audio")
	if a == nil || !a.AACProgramConfig || a.AACConfigPTS == nil || a.AACSourceStart == nil {
		return "", 0, false
	}
	lead := *a.AACConfigPTS - *a.AACSourceStart
	if lead < 0 || lead > 2_000_000 || s.Start > 24*time.Hour {
		return "", 0, false
	}
	prelude = time.Duration(lead)*time.Microsecond + time.Second
	source := s.Input
	if len(source) == 0 || len(source) > 8192 || strings.ContainsAny(source, "\r\n\x00") {
		return "", 0, false
	}
	if !isNetworkURL(source) {
		p, err := filepath.Abs(source)
		if err != nil || strings.ContainsAny(p, "\r\n\x00") {
			return "", 0, false
		}
		source = "file:" + filepath.ToSlash(p)
	}
	quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'" }
	headers := s.commandHeaders()
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var opts strings.Builder
	opts.WriteString("option format_whitelist mpegts\n")
	if isNetworkURL(s.Input) {
		opts.WriteString("option rw_timeout 30000000\noption initial_request_size 262144\noption request_size 262144\noption reconnect 1\noption reconnect_on_network_error 1\noption reconnect_on_http_error 416,5xx\noption reconnect_max_retries 2\noption reconnect_delay_max 4\n")
	}
	custom := 0
	for _, k := range keys {
		v := headers[k]
		if v == "" {
			continue
		}
		if strings.ContainsAny(k+v, "\r\n\x00") {
			return "", 0, false
		}
		switch strings.ToLower(k) {
		case "user-agent":
			fmt.Fprintf(&opts, "option user_agent %s\n", quote(v))
		case "referer":
			fmt.Fprintf(&opts, "option referer %s\n", quote(v))
		default:
			custom++
			if custom > 1 {
				// FFconcat option values cannot safely carry CRLF-separated
				// headers; never silently drop a required source header.
				return "", 0, false
			}
			fmt.Fprintf(&opts, "option headers %s\n", quote(k+": "+v))
		}
	}
	outpoint := float64(*a.AACSourceStart)/1e6 + prelude.Seconds()
	inpoint := float64(*a.AACSourceStart)/1e6 + s.Start.Seconds()
	manifest := fmt.Sprintf("ffconcat version 1.0\nfile %s\n%soutpoint %.6f\nduration %.6f\nfile %s\n%sinpoint %.6f\n", quote(source), opts.String(), outpoint, prelude.Seconds(), quote(source), opts.String(), inpoint)
	// Keep an entire URI well below the stderr line buffer so redaction can
	// never expose fragments of a split source URL or input header.
	if len(manifest) > 8192 {
		return "", 0, false
	}
	return "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte(manifest)), prelude, true
}
