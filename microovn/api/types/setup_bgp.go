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

const BgpConfigKeyPrefix = "microovn-bgp"

const BgpConfigKeyCluster = "microovn-bgp-cluster"

const BgpConfigKeyInterface = "microovn-bgp-interface"

const BgpConfigKeyBridge = "microovn-bgp-br"

const BgpConfigKeyAsn = "microovn-bgp-asn"

const BgpConfigKeyAsnRange = "microovn-bgp-asn-range"

const BgpConfigKeyConnectionIP4Range = "microovn-bgp-connection-ip4-range"
