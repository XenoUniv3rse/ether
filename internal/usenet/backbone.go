package usenet

import "strings"

// backbone is the provider's article store name, normalized; "" when the
// account is not grouped with others.
func (p *pool) backbone() string {
	return strings.ToLower(strings.TrimSpace(p.cfg.Backbone))
}

// noteMissing records that p answered "no such article" for id, and that
// every other enabled account on p's backbone did too: accounts sharing an
// article store give the same answer, so asking them again only costs a
// round trip (often a full connection's worth of latency per segment of an
// old post). It returns the keys of those other accounts.
//
// Only a real 430/451 goes through here. A corrupt copy or a slot mismatch
// is specific to one server's reply and is not shared.
func (f *Fetcher) noteMissing(id string, p *pool) []string {
	f.misses.add(id, p.key)
	b := p.backbone()
	if b == "" {
		return nil
	}
	f.mu.RLock()
	pools := f.pools
	f.mu.RUnlock()
	var same []string
	for _, o := range pools {
		if o != p && o.backbone() == b {
			f.misses.add(id, o.key)
			same = append(same, o.key)
		}
	}
	return same
}
