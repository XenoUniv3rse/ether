package livetv

import (
	"net/http"
	"sync"
)

func providerInputKey(raw string, headers http.Header, rawHeaders map[string]string) string {
	return tapKey(raw+"\x00"+tapKey("", redirectMediaHeaders(rawHeaders)), headers)
}

func (r *tapRegistry) unmanagedInUseLocked(source string) int {
	n := 0
	for t := range r.bySource[source] {
		if !t.managed {
			n++
		}
	}
	return n
}

func (r *tapRegistry) inUseLocked(source string) int {
	return len(r.inputs[source]) + r.unmanagedInUseLocked(source) + r.external[source]
}

func (r *tapRegistry) inUse(source string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inUseLocked(source)
}

// holdProviderInput accounts for one logical upstream, including the gaps
// between HLS requests. Probes, relays and passthrough of the same input
// share it. Finite archive inputs use a distinct key for every range.
func (s *Service) holdProviderInput(source, key string, limit int) (func(), error) {
	if source == "" {
		return nil, nil
	}
	r := &s.inputRelay.taps
	r.mu.Lock()
	keys := r.inputs[source]
	if keys[key] == 0 && limit > 0 && r.inUseLocked(source) >= limit {
		r.mu.Unlock()
		return nil, ErrProviderBusy
	}
	if keys == nil {
		keys = map[string]int{}
		if r.inputs == nil {
			r.inputs = map[string]map[string]int{}
		}
		r.inputs[source] = keys
	}
	keys[key]++
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			keys[key]--
			if keys[key] == 0 {
				delete(keys, key)
			}
			if len(keys) == 0 {
				delete(r.inputs, source)
			}
			r.mu.Unlock()
		})
	}, nil
}
