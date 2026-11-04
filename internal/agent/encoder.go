package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// UI message stream encoder. Chunk fields match ai@6 uiMessageChunkSchema
// (the version @braedonsaunders/appkit-ai@3.0.0 decodes with).

// Chunk is one stream chunk, marshalled as an SSE data line.
type Chunk map[string]any

func chunkStart(messageID string) Chunk {
	return Chunk{"type": "start", "messageId": messageID}
}
func chunkStartStep() Chunk  { return Chunk{"type": "start-step"} }
func chunkFinishStep() Chunk { return Chunk{"type": "finish-step"} }
func chunkFinish(reason string) Chunk {
	return Chunk{"type": "finish", "finishReason": reason}
}
func chunkError(text string) Chunk { return Chunk{"type": "error", "errorText": text} }
func chunkTextStart(id string) Chunk {
	return Chunk{"type": "text-start", "id": id}
}
func chunkTextDelta(id, delta string) Chunk {
	return Chunk{"type": "text-delta", "id": id, "delta": delta}
}
func chunkTextEnd(id string) Chunk { return Chunk{"type": "text-end", "id": id} }
func chunkToolInputStart(callID, name string) Chunk {
	return Chunk{"type": "tool-input-start", "toolCallId": callID, "toolName": name}
}
func chunkToolInputDelta(callID, delta string) Chunk {
	return Chunk{"type": "tool-input-delta", "toolCallId": callID, "inputTextDelta": delta}
}
func chunkToolInputAvailable(callID, name string, input any) Chunk {
	return Chunk{"type": "tool-input-available", "toolCallId": callID, "toolName": name, "input": input}
}
func chunkToolOutputAvailable(callID string, output any) Chunk {
	return Chunk{"type": "tool-output-available", "toolCallId": callID, "output": output}
}
func chunkToolOutputError(callID, text string) Chunk {
	return Chunk{"type": "tool-output-error", "toolCallId": callID, "errorText": text}
}

// writeSSE writes one chunk and flushes.
func writeSSE(w http.ResponseWriter, c Chunk) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func writeSSEDone(w http.ResponseWriter) {
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// streamHeaders sets the AI SDK UI message stream headers.
func streamHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("x-vercel-ai-ui-message-stream", "v1")
	h.Set("X-Accel-Buffering", "no")
}

// sseStream serialises one turn's writes: chunks from the turn, heartbeat
// comments, and the final [DONE]. Once closed it drops every write, so a
// detached turn never touches a finished response.
type sseStream struct {
	mu       sync.Mutex
	w        http.ResponseWriter
	closed   bool
	detached bool
}

func newSSEStream(w http.ResponseWriter) *sseStream { return &sseStream{w: w} }

// detach makes write errors silent: the client may be gone, the turn is not.
func (s *sseStream) detach() {
	s.mu.Lock()
	s.detached = true
	s.mu.Unlock()
}

func (s *sseStream) emit(c Chunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	err := writeSSE(s.w, c)
	if s.detached {
		return nil
	}
	return err
}

// heartbeat writes a comment every interval until the returned stop runs.
func (s *sseStream) heartbeat(every time.Duration) (stop func()) {
	quit := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-t.C:
				s.mu.Lock()
				if !s.closed {
					_, _ = fmt.Fprint(s.w, ": keep-alive\n\n")
					if f, ok := s.w.(http.Flusher); ok {
						f.Flush()
					}
				}
				s.mu.Unlock()
			}
		}
	}()
	return func() { once.Do(func() { close(quit) }) }
}

// done writes the stream terminator and closes the stream.
func (s *sseStream) done() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		writeSSEDone(s.w)
	}
	s.closed = true
}

// close stops all further writes; the handler is about to return.
func (s *sseStream) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}
