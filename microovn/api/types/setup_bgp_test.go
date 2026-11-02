package types

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSetupBgpRequest_JSONRoundTrip(t *testing.T) {
	req := SetupBgpRequest{
		ExtraBgpConfig: ExtraBgpConfig{
			ExternalConnection: "eth0",
			Asn:                "4200000001",
		},
		ConnectionIP4Range: "10.0.0.1-10.0.0.10",
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded SetupBgpRequest
	err = json.Unmarshal(data, &decoded)
	if err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if decoded.ExternalConnection != "eth0" {
		t.Errorf("expected ExternalConnection 'eth0', got '%s'", decoded.ExternalConnection)
	}
	if decoded.Asn != "4200000001" {
		t.Errorf("expected Asn '4200000001', got '%s'", decoded.Asn)
	}
	if decoded.ConnectionIP4Range != "10.0.0.1-10.0.0.10" {
		t.Errorf("expected ConnectionIP4Range '10.0.0.1-10.0.0.10', got '%s'", decoded.ConnectionIP4Range)
	}
}

func TestSetupBgpRequest_JSON_IncludesEmbeddedFields(t *testing.T) {
	req := SetupBgpRequest{
		ExtraBgpConfig: ExtraBgpConfig{
			Bridge:   "br0",
			Asn:      "4200000002",
			AsnRange: [2]uint64{4210000000, 4294967294},
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	jsonStr := string(data)
	if !strings.Contains(jsonStr, `"bridge"`) {
		t.Error("JSON output missing 'bridge' field from embedded ExtraBgpConfig")
	}
	if !strings.Contains(jsonStr, `"br0"`) {
		t.Error("JSON output missing bridge value 'br0'")
	}
	if !strings.Contains(jsonStr, `"asn_range"`) {
		t.Error("JSON output missing 'asn_range' field from embedded ExtraBgpConfig")
	}
}

func TestSetupBgpRequest_EmptyJSON(t *testing.T) {
	var decoded SetupBgpRequest
	err := json.Unmarshal([]byte(`{}`), &decoded)
	if err != nil {
		t.Fatalf("failed to unmarshal empty JSON: %v", err)
	}

	if decoded.ExternalConnection != "" {
		t.Error("expected empty ExternalConnection")
	}
	if decoded.Bridge != "" {
		t.Error("expected empty Bridge")
	}
	if decoded.Asn != "" {
		t.Error("expected empty Asn")
	}
	if decoded.ConnectionIP4Range != "" {
		t.Error("expected empty ConnectionIP4Range")
	}
}

func TestSetupBgpRequest_MutualExclusion(t *testing.T) {
	tests := []struct {
		name       string
		req        SetupBgpRequest
		expectErr  bool
		wantErrMsg string
	}{
		{
			name:       "both empty",
			req:        SetupBgpRequest{},
			expectErr:  true,
			wantErrMsg: "either",
		},
		{
			name: "both set",
			req: SetupBgpRequest{
				ExtraBgpConfig: ExtraBgpConfig{
					ExternalConnection: "eth0",
					Bridge:             "br0",
				},
			},
			expectErr:  true,
			wantErrMsg: "mutually exclusive",
		},
		{
			name: "interface only",
			req: SetupBgpRequest{
				ExtraBgpConfig: ExtraBgpConfig{
					ExternalConnection: "eth0",
				},
			},
			expectErr: false,
		},
		{
			name: "bridge only",
			req: SetupBgpRequest{
				ExtraBgpConfig: ExtraBgpConfig{
					Bridge: "br0",
				},
			},
			expectErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.expectErr {
				if err == nil {
					t.Error("expected error but got none")
				} else if !strings.Contains(err.Error(), tc.wantErrMsg) {
					t.Errorf("expected error containing '%s', got '%s'", tc.wantErrMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestBgpConfigKeyConstants(t *testing.T) {
	if BgpConfigKeyCluster != "microovn-bgp-cluster" {
		t.Errorf("expected 'microovn-bgp-cluster', got '%s'", BgpConfigKeyCluster)
	}
	if BgpConfigKeyInterface != "microovn-bgp-interface" {
		t.Errorf("expected 'microovn-bgp-interface', got '%s'", BgpConfigKeyInterface)
	}
	if BgpConfigKeyBridge != "microovn-bgp-br" {
		t.Errorf("expected 'microovn-bgp-br', got '%s'", BgpConfigKeyBridge)
	}
	if BgpConfigKeyAsn != "microovn-bgp-asn" {
		t.Errorf("expected 'microovn-bgp-asn', got '%s'", BgpConfigKeyAsn)
	}
	if BgpConfigKeyAsnRange != "microovn-bgp-asn-range" {
		t.Errorf("expected 'microovn-bgp-asn-range', got '%s'", BgpConfigKeyAsnRange)
	}
	if BgpConfigKeyConnectionIP4Range != "microovn-bgp-connection-ip4-range" {
		t.Errorf("expected 'microovn-bgp-connection-ip4-range', got '%s'", BgpConfigKeyConnectionIP4Range)
	}
}
