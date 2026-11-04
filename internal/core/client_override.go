package core

// ClientCapabilityOverride is an owner edit of one registry client.
// Nil slices and zero numbers leave the registry value in place.
// A client that declares the same fact at runtime still wins.
type ClientCapabilityOverride struct {
	ID      string `json:"id,omitempty"`
	Product string `json:"product,omitempty"`
	// ProductExact marks a product-only override already scoped by the app editor
	// or migrated from the old substring matcher.
	ProductExact bool `json:"productExact,omitempty"`
	// ProductMatch preserves legacy substring selectors. New app edits are exact.
	ProductMatch string `json:"productMatch,omitempty"`
	// CopiedFrom identifies a migrated shared selector for the console badge.
	CopiedFrom string `json:"copiedFrom,omitempty"`
	Platform   string `json:"platform,omitempty"`
	Device     string `json:"device,omitempty"`
	// Version is the minimum client version (dotted numbers) this override requires.
	Version             string   `json:"version,omitempty"`
	VideoCodecs         []string `json:"videoCodecs,omitempty"`
	AudioCodecs         []string `json:"audioCodecs,omitempty"`
	Containers          []string `json:"containers,omitempty"`
	SubtitleCodecs      []string `json:"subtitleCodecs,omitempty"`
	DolbyVisionProfiles []int    `json:"dolbyVisionProfiles,omitempty"`
	MaxHeight           int      `json:"maxHeight,omitempty"`
	MaxBitrateKbps      int      `json:"maxBitrateKbps,omitempty"`
	HDR10               *bool    `json:"hdr10,omitempty"`
	DolbyVision         *bool    `json:"dolbyVision,omitempty"`
	Notes               string   `json:"notes,omitempty"`
}
