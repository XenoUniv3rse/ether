package core

import (
	"context"
	"sync/atomic"
)

type speculativeKey struct{}

// WithSpeculative marks ctx as speculative work: a warm-up for a title
// nobody has pressed Play on yet. The returned promote clears the mark
// when a Play takes the work over, so from then on it runs as a real play.
func WithSpeculative(ctx context.Context) (context.Context, func()) {
	flag := new(atomic.Bool)
	flag.Store(true)
	return context.WithValue(ctx, speculativeKey{}, flag), func() { flag.Store(false) }
}

// IsSpeculative reports whether ctx is still speculative work.
func IsSpeculative(ctx context.Context) bool {
	flag, _ := ctx.Value(speculativeKey{}).(*atomic.Bool)
	return flag != nil && flag.Load()
}

type explicitPlayKey struct{}

// playMark is one explicit-Play flag. A mark made for shared work (a
// preparation flight) also reads as set when the work it runs inside is.
type playMark struct {
	set    atomic.Bool
	parent *playMark
}

func (m *playMark) isSet() bool {
	for ; m != nil; m = m.parent {
		if m.set.Load() {
			return true
		}
	}
	return false
}

// WithExplicitPlay marks ctx as a viewer pressing Play. Work that runs on
// it, or on a context detached from it, reads ExplicitPlay as true.
func WithExplicitPlay(ctx context.Context) context.Context {
	m := &playMark{parent: playMarkFrom(ctx)}
	m.set.Store(true)
	return context.WithValue(ctx, explicitPlayKey{}, m)
}

// WithExplicitPlayLatch gives work several callers share its own mark. It
// reads as set when ctx is an explicit Play, and from the moment the
// returned latch is called (an explicit Play joined). It is never cleared.
func WithExplicitPlayLatch(ctx context.Context) (context.Context, func()) {
	m := &playMark{parent: playMarkFrom(ctx)}
	return context.WithValue(ctx, explicitPlayKey{}, m), func() { m.set.Store(true) }
}

// ExplicitPlay reports whether ctx is a viewer's explicit Play, or work an
// explicit Play joined. It is false unless WithExplicitPlay set it.
func ExplicitPlay(ctx context.Context) bool {
	return playMarkFrom(ctx).isSet()
}

func playMarkFrom(ctx context.Context) *playMark {
	m, _ := ctx.Value(explicitPlayKey{}).(*playMark)
	return m
}
