package transcode

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/subtitles"
)

// ServeSoftSubtitle attaches to the ASS output of the active encoder. The
// output belongs to the run, not the HTTP request, so retries do not open a
// second source reader. Encoder replacements are followed on the same stream.
func (s *Session) ServeSoftSubtitle(w http.ResponseWriter, req *http.Request, index int) bool {
	s.mu.Lock()
	if s.closed || s.softSubtitleStream == nil || *s.softSubtitleStream != index {
		s.mu.Unlock()
		return false
	}
	track := findStream(s.info, s.softSubtitleInputIndex, "subtitle")
	plain := track != nil && (strings.EqualFold(track.Codec, "subrip") || strings.EqualFold(track.Codec, "srt"))
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/x-ssa")
	flusher, _ := w.(http.Flusher)
	if plain {
		// SRT has no embedded ASS styles. Start the parser immediately even
		// while the encoder is probing a slow source.
		if _, err := io.WriteString(w, subtitles.ASSHeader); err != nil {
			return true
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	keepalive := time.NewTicker(4 * time.Second)
	defer keepalive.Stop()
	var file *os.File
	defer func() {
		if file != nil {
			_ = file.Close()
		}
	}()
	var seq int
	var header []byte
	stripHeader := plain
	buf := make([]byte, 32<<10)
	for {
		s.mu.Lock()
		closed, r := s.closed, s.cur
		s.mu.Unlock()
		if closed {
			return true
		}
		if r != nil && r.spec.SoftSubtitlePath == "" {
			return true // the selected track is absent after source failover
		}
		if r != nil && r.seq != seq {
			if file != nil {
				_ = file.Close()
				file = nil
			}
			seq = r.seq
			header = nil
			stripHeader = plain
		}
		if r != nil && file == nil && r.spec.SoftSubtitlePath != "" {
			f, err := os.Open(r.spec.SoftSubtitlePath)
			if err == nil {
				file = f
			} else if !errors.Is(err, os.ErrNotExist) {
				return true
			}
		}
		if file != nil {
			n, err := file.Read(buf)
			if n > 0 {
				data := buf[:n]
				if stripHeader {
					header = append(header, data...)
					if len(header) > 1<<20 {
						return true
					}
					events := bytes.Index(header, []byte("[Events]"))
					if events < 0 {
						continue
					}
					line := bytes.IndexByte(header[events:], '\n')
					if line < 0 {
						continue
					}
					format := events + line + 1
					end := bytes.IndexByte(header[format:], '\n')
					if end < 0 {
						continue
					}
					data = header[format+end+1:]
					header = nil
					stripHeader = false
				}
				if len(data) == 0 {
					continue
				}
				if _, e := w.Write(data); e != nil {
					return true
				}
				if flusher != nil {
					flusher.Flush()
				}
				continue
			}
			if err != nil && err != io.EOF {
				return true
			}
		}
		s.mu.Lock()
		done := r != nil && s.cur == r && r.finished && !s.pendingStart && s.resumeAt < 0
		s.mu.Unlock()
		if done {
			return true
		}
		select {
		case <-req.Context().Done():
			return true
		case <-ticker.C:
		case <-keepalive.C:
			if _, err := io.WriteString(w, "\n"); err != nil {
				return true
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}
