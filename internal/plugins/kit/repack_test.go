package kit

import (
	"context"
	"testing"
)

// A package whose manifest still names its TypeScript source (a
// source-containing .etherpkg, which LoadPkgBytes leaves as is) packs to
// an archive whose entry is the compiled bundle, so the host loads it
// once instead of wrapping the bundle again. Repacking that archive stays
// loadable too.
func TestPackSourceEntryArchiveLoadsAndRepacks(t *testing.T) {
	files, err := GenerateTemplateFiles(TemplateOptions{Kind: "livetv_source", Lang: "ts", ID: "com.example.repack", Name: "Repack check"})
	if err != nil {
		t.Fatal(err)
	}
	src := findTSEntry(files, bundleEntry)
	if src == "" {
		t.Fatal("template has no TypeScript source")
	}
	manRaw, err := withEntry(files["manifest.json"], src)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := FromFiles(manRaw, files)
	if err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= 2; round++ {
		archive, err := Pack(pkg)
		if err != nil {
			t.Fatalf("pack %d: %v", round, err)
		}
		if pkg, err = LoadPkgBytes(archive); err != nil {
			t.Fatalf("load %d: %v", round, err)
		}
		if got := pkg.Manifest.Ether.Runtime.Entry; got != bundleEntry {
			t.Fatalf("pack %d entry %q, want %q", round, got, bundleEntry)
		}
		if _, ok := pkg.Files[src]; !ok {
			t.Fatalf("pack %d dropped the source %s", round, src)
		}
		if rep := Check(context.Background(), pkg); !rep.OK {
			t.Fatalf("pack %d does not load and run:\n%s", round, rep.Text())
		}
	}
}
