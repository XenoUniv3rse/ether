package livetv

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// Viewers already sharing an input count once. Waiters never claim a slot
// until that input closes, and a cancelled waiter leaves the queue in order.
func TestProviderInputCapacityWaitInvariant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Service{}
		const provider = "provider"
		first, err := s.holdProviderInput(provider, "first", 1)
		if err != nil {
			t.Fatal(err)
		}
		shared, err := s.holdProviderInput(provider, "first", 1)
		if err != nil {
			t.Fatal(err)
		}
		type result struct {
			release func()
			err     error
		}
		start := func(ctx context.Context, key string, wait time.Duration) <-chan result {
			done := make(chan result, 1)
			go func() {
				var release func()
				err := s.providerWait.wait(ctx, provider, wait, ErrProviderBusy, func(context.Context) error {
					var err error
					release, err = s.holdProviderInput(provider, key, 1)
					return err
				})
				done <- result{release, err}
			}()
			synctest.Wait()
			return done
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancelled := start(ctx, "cancelled", time.Second)
		next := start(t.Context(), "next", time.Second)
		timedOut := start(t.Context(), "timeout", time.Second)
		first()
		synctest.Wait()
		select {
		case <-next:
			t.Fatal("disconnecting one viewer freed another viewer's slot")
		default:
		}
		cancel()
		if got := <-cancelled; !errors.Is(got.err, context.Canceled) || got.release != nil {
			t.Fatal("cancelled waiter took capacity", got.err)
		}
		shared()
		acquired := <-next
		if acquired.err != nil || acquired.release == nil {
			t.Fatal("first surviving waiter did not receive the freed slot", acquired.err)
		}
		if got := <-timedOut; !errors.Is(got.err, ErrProviderBusy) || got.release != nil {
			t.Fatal("waiter bypassed the active reservation", got.err)
		}
		acquired.release()
		acquired.release()
		if s.inputRelay.taps.inUse(provider) != 0 {
			t.Fatal("provider capacity leaked")
		}
	})
}
