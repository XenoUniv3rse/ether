package config

import "testing"

func TestUpgradeRetiresPluginBudgetPreservesMediaBudget(t *testing.T) {
	raw := []byte(`{"cache":{"budgetBytes":98765,"pluginLibraryBudgetBytes":111},"plugins":{"runtimeLimits":{"sharedCacheBytes":222,"fetchBytes":123}}}`)
	c, _, err := decodeOverDefaults(Config{}, raw)
	if err != nil || c.EffectiveCache().BudgetBytes != 98765 || c.Plugins.RuntimeLimits.FetchBytes != 123 {
		t.Fatalf("budget %d fetch %d err %v", c.EffectiveCache().BudgetBytes, c.Plugins.RuntimeLimits.FetchBytes, err)
	}
}
