package app

import (
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// splitAppOverrides preserves the order of overlapping edits. A product-only
// legacy selector used substring matching; expand it to known exact identities.
func splitAppOverrides(in []core.ClientCapabilityOverride) ([]core.ClientCapabilityOverride, int) {
	var products []string
	seen := make(map[string]bool)
	for _, app := range builtinApps() {
		product := app.Product
		if product == "" {
			product = app.Name
		}
		key := strings.ToLower(strings.TrimSpace(product))
		if !seen[key] {
			seen[key] = true
			products = append(products, product)
		}
	}
	out := make([]core.ClientCapabilityOverride, 0, len(in))
	changed := 0
	for _, o := range in {
		if o.ProductExact || o.ID != "" || o.Device != "" || o.Platform != "" || strings.TrimSpace(o.Product) == "" {
			out = append(out, o)
			continue
		}
		needle := strings.ToLower(strings.TrimSpace(o.Product))
		var matches []string
		for _, product := range products {
			if strings.Contains(strings.ToLower(product), needle) {
				matches = append(matches, product)
			}
		}
		if len(matches) == 0 || (len(matches) == 1 && strings.EqualFold(strings.TrimSpace(o.Product), matches[0])) {
			out = append(out, o)
			continue
		}
		changed++
		// The old selector also matched a client sending the generic product
		// itself (for example older Plex for Android builds). Keep that exact
		// identity alongside the distinct known products.
		original := o
		original.ProductExact = true
		out = append(out, original)
		for _, product := range matches {
			if strings.EqualFold(strings.TrimSpace(o.Product), product) {
				continue
			}
			copy := o
			copy.Product = product
			copy.ProductExact = true
			out = append(out, copy)
		}
	}
	return out, changed
}

func (a *App) migrateAppOverrides() error {
	if _, count := splitAppOverrides(a.cfg.Read().Clients.Overrides); count == 0 {
		return nil
	}
	count := 0
	err := a.cfg.Update(func(c *config.Config) error {
		c.Clients.Overrides, count = splitAppOverrides(c.Clients.Overrides)
		return nil
	})
	if err == nil && count > 0 {
		a.log.Info("expanded legacy app capability overrides", "count", count)
	}
	return err
}
