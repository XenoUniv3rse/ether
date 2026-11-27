package plex

import "testing"

func TestRouteInventory(t *testing.T) {
	inv := RouteInventory()
	if !inv.Available || len(inv.Routes) == 0 {
		t.Fatal(inv)
	}
	found := false
	wildcardCount := 0
	for _, r := range inv.Routes {
		if r.Pattern == "/:/timeline" {
			wildcardCount++
			if r.Method != "*" {
				t.Errorf("timeline registration expanded into %s", r.Method)
			}
		}
		if r.Method == "GET" && r.Pattern == "/identity" {
			found = true
		}
	}
	if wildcardCount != 1 {
		t.Errorf("got %d timeline registrations, want 1", wildcardCount)
	}
	if !found {
		t.Fatal("missing GET /identity")
	}
}
