package types

// EnableBirdRequest carries the structured underlay configuration for
// `microovn enable bird` (or a raw BIRD config when --stdin was used).
type EnableBirdRequest struct {
	// Uplinks are the repeatable host interfaces for the fabric sessions.
	Uplinks []string `json:"uplinks,omitempty" yaml:"uplinks,omitempty"`
	// Config is the optional --config key=value map (asn, router_id, ula).
	Config map[string]string `json:"config,omitempty" yaml:"config,omitempty"`
	// Raw is a raw BIRD configuration (the --stdin passthrough).
	Raw string `json:"raw,omitempty" yaml:"raw,omitempty"`
}

// EnableBirdResponse is the response to `microovn enable bird`.
type EnableBirdResponse struct {
	Success bool   `json:"success" yaml:"success"`
	Message string `json:"message" yaml:"message"`
}
