package admin_test

import (
	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"net/http"
	"testing"
)

func TestPluginLibraryBudgetPatch(t *testing.T) {
	cfg := openConfig(t, t.TempDir())
	h := admin.NewHandler(admin.Deps{Config: cfg, Logger: quietLog()})
	path := "/ether/api/config/cache"
	if err := cfg.Update(func(c *config.Config) error { c.Plugins.RuntimeLimits.FetchBytes = 20 << 30; return nil }); err != nil {
		t.Fatal(err)
	}
	putJSON(t, h, http.MethodPut, path, `{"pluginLibraryBudgetBytes":51539607552}`, "", http.StatusOK)
	var out struct{ Value config.Cache }
	decode(t, putJSON(t, h, http.MethodGet, path, "", "", http.StatusOK), &out)
	if out.Value.PluginLibraryBudgetBytes != 48<<30 || cfg.Read().Plugins.RuntimeLimits.FetchBytes != 20<<30 {
		t.Fatal("cache save changed plugin limit or lost budget")
	}
	putJSON(t, h, http.MethodPut, path, `{"budgetBytes":1073741824}`, "", http.StatusOK)
	putJSON(t, h, http.MethodPut, path, `{"pluginLibraryBudgetBytes":-1}`, "", http.StatusBadRequest)
	if cfg.Read().Cache.PluginLibraryBudgetBytes != 48<<30 {
		t.Fatal("unrelated save or invalid write changed library budget")
	}
	putJSON(t, h, http.MethodPut, path, `{"pluginLibraryBudgetBytes":0}`, "", http.StatusOK)
	if cfg.Read().Cache.PluginLibraryBudgetBytes != 0 || cfg.Read().Cache.BudgetBytes != 1<<30 {
		t.Fatal("failed to reset library budget independently")
	}
}
