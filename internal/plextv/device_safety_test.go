package plextv

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRemoveDeviceRequiresUniqueIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, identity, devices, wantDelete string
		wantErr                             bool
	}{
		{"empty", "", `<Device id="1"/>`, "", true},
		{"ambiguous", "ether", `<Device id="1" clientIdentifier="ether"/><Device id="2" clientIdentifier="ether"/>`, "", true},
		{"invalid id", "ether", `<Device id="-1" clientIdentifier="ether"/>`, "", true},
		{"unrelated PMS survives", "ether", `<Device id="10" clientIdentifier="pms"/><Device id="20" clientIdentifier="ether"/>`, "/devices/20.xml", false},
		{"no match", "ether", `<Device id="10" clientIdentifier="pms"/>`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deleted := ""
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deleted = r.URL.Path
					return
				}
				fmt.Fprintf(w, "<MediaContainer>%s</MediaContainer>", tc.devices)
			}))
			defer srv.Close()
			c, err := New(Options{BaseURL: srv.URL, Identity: Identity{ClientIdentifier: tc.identity}})
			if err != nil {
				t.Fatal(err)
			}
			removed, err := c.RemoveDevice(context.Background(), "token")
			if (err != nil) != tc.wantErr || deleted != tc.wantDelete || removed != (tc.wantDelete != "") {
				t.Fatalf("removed=%v err=%v deleted=%q", removed, err, deleted)
			}
		})
	}
}
