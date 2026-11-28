package config

import (
	"testing"
)

func TestUpgradePluginLibraryBudget(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want int64
	}{
		{`{"plugins":{"runtimeLimits":{"sharedCacheBytes":51539607552,"fetchBytes":123}}}`, 48 << 30},
		{`{"cache":{"pluginLibraryBudgetBytes":0},"plugins":{"runtimeLimits":{"sharedCacheBytes":51539607552,"fetchBytes":123}}}`, 0},
	} {
		c, _, err := decodeOverDefaults(Config{}, []byte(tt.raw))
		if err != nil || c.Cache.PluginLibraryBudgetBytes != tt.want || c.Plugins.RuntimeLimits.FetchBytes != 123 {
			t.Fatalf("decoded budget = %d, fetch = %d, err %v", c.Cache.PluginLibraryBudgetBytes, c.Plugins.RuntimeLimits.FetchBytes, err)
		}
		c.Cache.Storage = "shared"
		c.SetCachePolicy(c.Cache)
		if c.SharedCache.PluginLibraryBudgetBytes != 0 || c.EffectiveCache().PluginLibraryBudgetBytes != tt.want {
			t.Fatal("shared pool changed node-local plugin cache budget")
		}
	}
}
