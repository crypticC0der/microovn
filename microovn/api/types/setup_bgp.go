package types

import "fmt"

type SetupBgpRequest struct {
	ExtraBgpConfig
	ConnectionIP4Range string `json:"connection_ip4_range,omitempty" yaml:"connection_ip4_range,omitempty"`
}

func (r *SetupBgpRequest) Validate() error {
	if r.ExternalConnection == "" && r.Bridge == "" {
		return fmt.Errorf("either --interface or --br must be provided")
	}
	if r.ExternalConnection != "" && r.Bridge != "" {
		return fmt.Errorf("--interface and --br are mutually exclusive")
	}
	return nil
}

type SetupBgpResponse struct {
	Success bool     `json:"success" yaml:"success"`
	Message string   `json:"message" yaml:"message"`
	Errors  []string `json:"errors,omitempty" yaml:"errors,omitempty"`
}

// SetupBirdRequest carries a raw BIRD configuration for `microovn setup bird`.
type SetupBirdRequest struct {
	// Config is the raw BIRD configuration text (passed on stdin to the CLI).
	Config string `json:"config" yaml:"config"`
}

// SetupBirdResponse is the response to `microovn setup bird`.
type SetupBirdResponse struct {
	Success bool   `json:"success" yaml:"success"`
	Message string `json:"message" yaml:"message"`
}

const BgpConfigKeyPrefix = "microovn-bgp"

// BgpConfigKeyCluster is the dqlite key that marks BGP as being set up
// cluster-wide (via `microovn setup bgp`). It stays in the dqlite store only.
const BgpConfigKeyCluster = "microovn-bgp-cluster"

const BgpConfigKeyInterface = "microovn-bgp-interface"

const BgpConfigKeyBridge = "microovn-bgp-br"

// BgpConfigKeyAsn is the dqlite key for the cluster-wide local ASN chosen by
// `microovn setup bgp`. NOTE: the same string "microovn-bgp-asn" is also read
// as an Open_vSwitch external-id (a node-local knob) by the managed and attach
// paths — the two stores are intentionally distinct and must not be confused.
const BgpConfigKeyAsn = "microovn-bgp-asn"

const BgpConfigKeyAsnRange = "microovn-bgp-asn-range"

const BgpConfigKeyConnectionIP4Range = "microovn-bgp-connection-ip4-range"
