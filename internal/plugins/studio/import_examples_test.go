package studio

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/kit"
)

// TestImportShippedExamples opens every shipped hosted SDK example in
// Studio after installing it the way an owner would, and requires a draft
// that compiles, runs in the dev sandbox and installs again. Examples
// whose source imports the SDK from outside the package only run from
// their installed bundle, so the draft must keep that entry.
func TestImportShippedExamples(t *testing.T) {
	dirs, err := filepath.Glob("../../../sdk/examples/*/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	more, _ := filepath.Glob("../../../sdk/examples/*/*/manifest.json")
	dirs = append(dirs, more...)
	ctx := context.Background()
	seen := 0
	for _, manPath := range dirs {
		dir := filepath.Dir(manPath)
		raw, err := os.ReadFile(manPath)
		if err != nil {
			t.Fatal(err)
		}
		man, err := plugins.ParseManifest(raw)
		if err != nil || man.RuntimeKind() != "hosted" || man.Ether.Runtime.Entry == "" {
			continue
		}
		seen++
		t.Run(filepath.ToSlash(dir[len("../../../sdk/examples/"):]), func(t *testing.T) {
			st, mgr, _ := newTestStudio(t)
			pkg, err := kit.LoadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			archive, err := kit.Pack(pkg)
			if err != nil {
				t.Fatal(err)
			}
			preview, err := mgr.PreviewInstall(ctx, plugins.Source{Kind: "pkg", Pkg: archive})
			if err != nil {
				t.Fatal(err)
			}
			grants := plugins.Grants{Viewers: []int64{1}, Folders: []string{"/media"}}
			if _, err := mgr.Install(ctx, preview, plugins.InstallOptions{Grants: grants}); err != nil {
				t.Fatalf("install shipped package: %v", err)
			}
			proj, err := st.ImportPlugin(ctx, man.ID)
			if err != nil {
				t.Fatal(err)
			}
			comp := st.CompileFiles(proj.Files, "")
			if !comp.OK || len(comp.compiledJS) == 0 {
				t.Fatalf("imported draft does not compile: %+v", comp.Diagnostics)
			}
			if _, err := st.RunProject(ctx, proj.ID, RunRequest{Method: "GET", Path: "/ether/health"}); err != nil {
				t.Fatalf("imported draft does not run: %v", err)
			}
			rec, err := st.InstallProject(ctx, proj.ID, plugins.InstallOptions{Grants: grants})
			if err != nil {
				t.Fatalf("imported draft does not reinstall: %v", err)
			}
			if rec.Manifest.Ether.Runtime.Entry != installBundlePath {
				t.Fatalf("reinstalled entry %q", rec.Manifest.Ether.Runtime.Entry)
			}
			if _, err := mgr.Call(ctx, rec.ID, plugins.Request{Method: "GET", Path: "/ether/health"}); err != nil {
				t.Fatalf("reinstalled plugin does not load: %v", err)
			}
		})
	}
	if seen < 10 {
		t.Fatalf("found only %d hosted examples", seen)
	}
}
