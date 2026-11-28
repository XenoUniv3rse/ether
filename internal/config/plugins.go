package config

import "strings"

// OfficialCatalogueURL is the Ether plugin marketplace: the signed index
// built from https://github.com/braedonsaunders/ether-plugins, where plugin
// authors submit entries by pull request.
const OfficialCatalogueURL = "https://braedonsaunders.github.io/ether-plugins/index.json"

// OfficialCataloguePublicKey verifies OfficialCatalogueURL's index.json.sig.
const OfficialCataloguePublicKey = "vy26IJ+BfNVKqj9jHhrSszio24Jab76fC2L33cfBEm8="

// Plugins configures the signed plugin catalogue (SPEC WS12). With no
// IndexURL Ether reads the official marketplace; an owner who runs their
// own catalogue sets both IndexURL and the PublicKey that signs it.
type Plugins struct {
	RuntimeLimits    PluginRuntimeLimits            `json:"runtimeLimits,omitempty"`
	RuntimeOverrides map[string]PluginRuntimeLimits `json:"runtimeOverrides,omitempty"`
	// IndexURL is the URL of index.json (with a sibling index.json.sig).
	// Empty means the official marketplace.
	IndexURL string `json:"indexUrl,omitempty"`
	// PublicKey is the base64-encoded ed25519 public key that signs index.json.
	PublicKey string `json:"publicKey,omitempty"`
}

// Catalogue returns the index URL and the key that must sign it: the
// owner's own catalogue when IndexURL is set, else the official marketplace.
// An owner URL is never paired with the official key.
func (p Plugins) Catalogue() (indexURL, publicKey string) {
	if url := strings.TrimSpace(p.IndexURL); url != "" {
		return url, strings.TrimSpace(p.PublicKey)
	}
	return OfficialCatalogueURL, OfficialCataloguePublicKey
}

// PluginRuntimeLimits are owner-controlled ceilings, not allocations. Zero
// inherits the default (globally) or the global ceiling (per plugin).
type PluginRuntimeLimits struct {
	// UploadBytes is the per-plugin staged request ceiling. Zero uses 64 MiB.
	UploadBytes int64 `json:"uploadBytes,omitempty"`
	// DurableBytes and DurableFiles override the per-plugin saved blob quota.
	DurableBytes int64 `json:"durableBytes,omitempty"`
	DurableFiles int   `json:"durableFiles,omitempty"`
	// DurableFreePercent sets the global free-space share for raised quotas. Zero uses 25%.
	DurableFreePercent   int   `json:"durableFreePercent,omitempty"`
	FetchBytes           int64 `json:"fetchBytes,omitempty"`
	ArchiveMemberBytes   int64 `json:"archiveMemberBytes,omitempty"`
	ArchiveExpandedBytes int64 `json:"archiveExpandedBytes,omitempty"`
	ArchiveMembers       int   `json:"archiveMembers,omitempty"`
	MaxItems             int   `json:"maxItems,omitempty"`
	// ListItems is how many titles one sync of each plugin list reads.
	// Globally it caps what lists ask for; per plugin it replaces it.
	ListItems   int    `json:"listItems,omitempty"`
	MemoryBytes uint64 `json:"memoryBytes,omitempty"`
	Deadline    string `json:"deadline,omitempty"`
	UnitBudget  int64  `json:"unitBudget,omitempty"` // zero means effectively unlimited
	PoolSize    int    `json:"poolSize,omitempty"`   // hosted instance pool ceiling; default 4, max 16
}
