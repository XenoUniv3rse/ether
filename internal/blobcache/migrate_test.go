package blobcache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportLegacyVerifiesBeforeRemovingAndPreservesAlias(t *testing.T) {
	ctx := context.Background()
	oldDir := t.TempDir()
	old, err := Open(oldDir, Options{SweepPeriod: -1})
	if err != nil {
		t.Fatal(err)
	}
	lib := old.Namespace("games", Quota{})
	ref, err := lib.Put(ctx, "game.iso", strings.NewReader("disc"), 4)
	if err != nil {
		t.Fatal(err)
	}
	other, err := lib.Put(ctx, "other.iso", strings.NewReader("bad!"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	h, _ := other.Hash()
	bad := filepath.Join(oldDir, "games", h[:2], h)
	if err := os.WriteFile(bad, []byte("oops"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	open := func() *Cache {
		c, e := Open(dir, Options{SweepPeriod: -1, Unified: true})
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	dst := open()
	if e := dst.ImportLegacy(ctx, oldDir); e == nil {
		t.Fatal("corrupt source accepted")
	}
	if _, e := os.Stat(bad); e != nil {
		t.Fatal("lost corrupt source", e)
	}
	good, _ := ref.Hash()
	if _, e := os.Stat(filepath.Join(oldDir, "games", good[:2], good)); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("verified source still present", e)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	dst = open()
	defer dst.Close()
	ns := dst.Namespace("games", Quota{})
	if r, ok := ns.LookupKey("game.iso"); !ok || r != ref {
		t.Fatalf("lost alias %v %v", r, ok)
	}
	r, e := ns.Open(ref)
	if e != nil {
		t.Fatal(e)
	}
	content, e := io.ReadAll(r)
	r.Close()
	if e != nil || string(content) != "disc" || r.Name() != "game.iso" {
		t.Fatalf("offline read: %s, %s, %v", content, r.Name(), e)
	}
	if err := os.WriteFile(bad, []byte("bad!"), 0600); err != nil {
		t.Fatal(err)
	}
	if e := dst.ImportLegacy(ctx, oldDir); e != nil {
		t.Fatal(e)
	}
	if e := dst.ImportLegacy(ctx, oldDir); e != nil {
		t.Fatal("repeat", e)
	}
}
