package resolver

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// PreferLocalFiles plays a granted local file before any fetched release,
// even when the local copy is below the profile's preferred quality.
func TestPreferLocalFilesRanksGrantedFileFirst(t *testing.T) {
	local := rel("local", "The.Signal.1999.480p.WEB-DL.AAC2.0.H264-GRP.mkv", 900)
	local.Protocol = "direct"
	local.GUID = "plugin:tv.ether.examples.local-folders:file:/media/library/The Signal (1999)/The.Signal.1999.480p.WEB-DL.AAC2.0.H264-GRP.mkv"
	local.IndexerID = "plugin:tv.ether.examples.local-folders"
	lazy := rel("lazy", "The.Signal.1999.480p.WEB-DL.AAC2.0.H264-GRP.mkv", 900)
	lazy.Protocol = "direct"
	lazy.GUID = "plugin:tv.ether.examples.local-folders:lazy:abc"
	remote := rel("remote", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H264-GRP", 6000)

	for _, prefer := range []bool{false, true} {
		cfg := testCfg()
		cfg.Resolver.PreferLocalFiles = prefer
		s := New(&fakeIndexer{}, fakeHealth{}, nil, cfg, Options{})
		req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}
		profile := s.ProfileFor(webCaps(), nil)
		releases := []core.Release{withParsed(remote), withParsed(lazy), withParsed(local)}
		cands, _, err := s.rankCandidates(t.Context(), req, searchRequestFor(req.Title), profile, releases, nil, "")
		s.Close()
		if err != nil || len(cands) != 3 {
			t.Fatalf("prefer=%t: cands=%d err=%v", prefer, len(cands), err)
		}
		want := "remote"
		if prefer {
			want = "local"
		}
		if got := cands[0].rel.ID; got != want {
			t.Fatalf("prefer=%t: first=%s, want %s", prefer, got, want)
		}
		if prefer && cands[1].rel.ID != "remote" {
			t.Fatalf("a lazy plugin stream is not a local file: second=%s", cands[1].rel.ID)
		}
	}
}

func TestGrantedLocalFile(t *testing.T) {
	for guid, want := range map[string]bool{
		"plugin:tv.ether.examples.local-folders:file:/media/a.mkv": true,
		"plugin:tv.ether.examples.local-folders:lazy:a":            false,
		"https://indexer.example/file:/a":                          false,
	} {
		if got := grantedLocalFile(core.Release{GUID: guid, Protocol: "direct"}); got != want {
			t.Errorf("%s: %t, want %t", guid, got, want)
		}
	}
	if grantedLocalFile(core.Release{GUID: "plugin:x:file:/a.mkv"}) {
		t.Error("a non-direct release is not a granted local file")
	}
}
