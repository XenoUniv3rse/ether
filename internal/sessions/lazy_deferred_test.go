package sessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestDeferredLazyFileAdmission(t *testing.T) {
	ctx := t.Context()
	db := store.OpenTest(t)
	now := time.Now()
	broker := &Broker{deps: BrokerDeps{Now: func() time.Time { return now }}}
	grants := &fakeLazyGrants{}
	stream := &lazyStreams{db: db, broker: broker, protocol: "stremio", grants: grants}
	key := bytes.Repeat([]byte{1}, 32)
	if err := EnableDeferredLazyFile(stream, &versionHandles{}, key); err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(frontend.PlanRequest{Device: core.Device{ID: "fixture"}, Protocol: "stremio", Item: core.MovieKey(1)})
	if err != nil {
		t.Fatal(err)
	}
	path, err := stream.sealDeferredFile(1, "grant-reference", request, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	token := path[len("/sessions/lazy/") : len(path)-len("/file")]
	durable := func() bool {
		t.Helper()
		var found bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions_lazy_streams WHERE hash=?)`, lazyHash(token)).Scan(&found); err != nil {
			t.Fatal(err)
		}
		return found
	}
	if durable() {
		t.Fatal("sealing unexpectedly admitted the capability")
	}
	foreign := &lazyStreams{protocol: "emby", broker: broker}
	if err := EnableDeferredLazyFile(foreign, &versionHandles{}, key); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := foreign.admitDeferredFile(ctx, token); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("cross-surface token accepted")
	}
	grants.revoked = true
	if _, _, _, _, err := stream.admitDeferredFile(ctx, token); !errors.Is(err, core.ErrUnauthorized) || durable() {
		t.Fatal("revoked grant admitted")
	}
	grants.revoked = false
	for range 2 {
		if _, _, _, _, err := stream.admitDeferredFile(ctx, token); err != nil {
			t.Fatal(err)
		}
		if !durable() {
			t.Fatal("redeemed capability missing")
		}
	}
}
