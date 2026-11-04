package debrid

import (
	"github.com/braedonsaunders/ether/internal/config"
	"testing"
)

func TestPluginProviderSurvivesConfigUpdateAndUnregisters(t *testing.T) {
	s := NewService(config.Debrid{}, Options{})
	p := &fakeProvider{kind: "plugin:example"}
	s.RegisterPluginProvider("example", "Example", p)
	if len(s.active()) != 1 || !s.Enabled() {
		t.Fatal("native provider was not registered")
	}
	s.UpdateConfig(config.Debrid{CachedOnly: true})
	if len(s.active()) != 1 || s.active()[0].p != p {
		t.Fatal("config update dropped native provider")
	}
	if len(s.Config().Providers) != 0 {
		t.Fatal("plugin was written into built-in config")
	}
	st := s.Stats()
	if len(st) != 1 || st[0].ID != "plugin:example/debrid" || st[0].Kind != "plugin:example" {
		t.Fatalf("stats: %+v", st)
	}
	s.RegisterPluginProvider("example", "Example", nil)
	if s.Enabled() || len(s.Stats()) != 0 {
		t.Fatal("disabled provider remains registered")
	}
	if !(Payload{Kind: "plugin:example"}).IsPluginProvider() || (Payload{Kind: "realdebrid"}).IsPluginProvider() {
		t.Fatal("incorrect HTTP trust classification")
	}
}
