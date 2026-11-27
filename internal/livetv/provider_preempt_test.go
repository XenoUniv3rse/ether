package livetv

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestIdleBroadcastProviderPreemption(t *testing.T) {
	for _, capacity := range []string{"source", "login", "tap"} {
		for _, state := range []string{"idle", "viewer", "starting", "spare", "yielding"} {
			t.Run(capacity+"/"+state, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					s := testService(t, Options{})
					defer s.Close()
					ctx := context.Background()
					limit := 1
					if state == "spare" {
						limit = 2
					}
					src := Source{ID: "provider", LoginPool: true, PlaylistURL: "https://provider.example/list", Account: &ProviderAccount{Status: "Active", MaxConnections: limit}}
					s.setSourceEntry(src.ID, authEntry{pool: &src, at: time.Now()})
					reserve := func(id string) (func(), error) {
						var release func()
						if capacity == "source" {
							var err error
							release, err = s.holdProviderInput(src.ID, id, limit)
							if err != nil {
								return nil, err
							}
						} else if capacity == "login" {
							lease, err := s.leaseLogin(ctx, src.ID, "https://provider.example/"+id, http.Header{})
							if err != nil {
								return nil, err
							}
							release = func() { s.releaseLogin(lease) }
						} else {
							g := &inputGrant{}
							if _, _, err := s.inputRelay.taps.acquire(id, src.ID, limit, g); err != nil {
								return nil, err
							}
							release = func() { s.inputRelay.taps.release(g) }
						}
						provider, err := s.trackBroadcastProvider(src.ID, id, id)
						if err != nil {
							release()
							return nil, err
						}
						return func() { release(); provider() }, nil
					}
					oldRelease, err := reserve("old")
					if err != nil {
						t.Fatal(err)
					}
					life, cancel := context.WithCancel(ctx)
					b := &broadcast{ctx: life, cancel: cancel, created: time.Now(), lastViewer: time.Now(), hadViewer: true}
					if state == "viewer" {
						b.viewers = 1
					} else if state == "starting" {
						b.lastViewer, b.hadViewer = time.Time{}, false
					}
					b.wg.Add(1)
					go func() {
						defer b.wg.Done()
						<-life.Done()
						// Model asynchronous decoder/input teardown with virtual time.
						time.Sleep(time.Second)
						oldRelease()
					}()
					s.broadcasts.outputs = map[broadcastKey]*broadcast{{channelID: "old"}: b}
					defer b.close()
					if state == "yielding" {
						b.closed = true
						cancel()
						delete(s.broadcasts.outputs, broadcastKey{channelID: "old"})
						s.broadcasts.retiring = map[*broadcast]broadcastKey{b: {channelID: "old"}}
					}
					attempts := 0
					var acquired func()
					err = s.withProviderCapacity(ctx, Channel{ID: "new", SourceID: src.ID}, StreamSource{}, func(context.Context) error {
						attempts++
						var err error
						acquired, err = reserve("new")
						return err
					})
					if state == "viewer" || state == "starting" {
						if !errors.Is(err, ErrProviderBusy) || attempts != 1 || b.closed {
							t.Fatalf("protected output: err=%v attempts=%d closed=%v", err, attempts, b.closed)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					defer acquired()
					if state == "spare" {
						if attempts != 1 || b.closed {
							t.Fatal("warm output was preempted with spare capacity")
						}
						return
					}
					if attempts != 2 || !b.closed {
						t.Fatalf("idle output not preempted: attempts=%d closed=%v", attempts, b.closed)
					}
					// Once admission has acquired the freed slot, concurrent attempts
					// must all observe it occupied, including across login leases.
					var wg sync.WaitGroup
					for i := range 4 {
						wg.Add(1)
						go func() {
							defer wg.Done()
							release, err := reserve(fmt.Sprintf("competitor-%d", i))
							if release != nil {
								release()
							}
							if !errors.Is(err, ErrProviderBusy) {
								t.Errorf("slot acquired twice: %v", err)
							}
						}()
					}
					wg.Wait()
				})
			})
		}
	}
}
