package usenet

import (
	"context"
	"fmt"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/usenet/nntptest"
)

func TestSpeedTestCandidateSelection(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{
		TLS: true, Username: "test-user", Password: "test-password",
		MaxConns: 1, RefuseCode: 481, RefuseText: "too many connections",
	})
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 4)}, Options{UseGroup: true, TLSConfig: s.ClientTLSConfig()})
	sparse := &nzb.File{Groups: []string{"alt.binaries.test"}}
	for i := range 12 {
		id := fmt.Sprintf("sparse-%d@test", i)
		sparse.Segments = append(sparse.Segments, nzb.Segment{MessageID: id})
		if i == 0 {
			addSeg(t, s, id, []byte("one available article"))
		}
	}
	doc := &nzb.NZB{Files: []*nzb.File{sparse}}
	r := f.SpeedTest(context.Background(), "p", []*nzb.NZB{doc}, false)
	if r.Status != "sample_missing" || r.Misses == 0 || r.MBps != 0 {
		t.Fatalf("sparse sample reported as a speed: %+v", r)
	}

	available := &nzb.File{Groups: []string{"alt.binaries.test"}}
	for i := range 12 {
		id := fmt.Sprintf("available-%d@test", i)
		available.Segments = append(available.Segments, nzb.Segment{MessageID: id})
		addSeg(t, s, id, []byte("available sample article"))
	}
	good := &nzb.NZB{Files: []*nzb.File{available}}
	r = f.SpeedTest(context.Background(), "p", []*nzb.NZB{doc, good}, false)
	if r.Status != "ok" || r.Misses == 0 || r.Bytes == 0 || r.MBps <= 0 || r.Connections == 0 || r.Refused == 0 || r.SpareOnly {
		t.Fatalf("test did not measure a usable sample with the remaining connection capacity: %+v", r)
	}
}
