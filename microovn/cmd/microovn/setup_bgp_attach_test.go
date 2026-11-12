package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/canonical/microovn/microovn/api/types"
)

func TestAttachTableRows(t *testing.T) {
	rows := attachTableRows([]types.BgpAttachNodePlan{
		{Member: "compute-0", MemberID: 4, Rack: "rack1", Address: "192.0.2.18/28", Peer: "65101@192.0.2.17", Attached: true, RouterPort: "lrp-c0"},
		{Member: "compute-1"},
	})
	want := [][]string{
		{"rack1", "65101@192.0.2.17", "compute-0", "4", "192.0.2.18/28", "true", "lrp-c0"},
		{"-", "-", "compute-1", "-", "-", "false", "-"},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(rows), len(want))
	}
	for i := range want {
		if !slices.Equal(rows[i], want[i]) {
			t.Errorf("row %d = %v, want %v", i, rows[i], want[i])
		}
	}
}

func TestPrintAttachResponseExitCode(t *testing.T) {
	if err := printAttachResponse(types.SetupBgpAttachResponse{Success: true, Message: "bgp-attach is not configured"}); err != nil {
		t.Fatalf("success must not return an error: %v", err)
	}
	err := printAttachResponse(types.SetupBgpAttachResponse{
		Success: false,
		Message: "bgp-attach applied, with 1 node error(s)",
		Nodes:   []types.BgpAttachNodePlan{{Member: "compute-1", Error: "no router port"}},
		Errors:  []string{"compute-1: no router port"},
	})
	if err == nil {
		t.Fatal("a response with node errors must return an error (non-zero exit)")
	}
}

func TestAttachGroupLine(t *testing.T) {
	line := attachGroupLine(types.BgpAttachGroup{
		SetupBgpAttachRequest: types.SetupBgpAttachRequest{Rack: "rack1", Peer: "65101@fe80::1", AdvertiseTable: 42, UplinkPattern: "*br-bgp"},
		Members:               []string{"c0", "c1"},
	})
	for _, want := range []string{"rack: rack1", "peer: 65101@fe80::1", "networks: (unnumbered)", "members: c0,c1"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q missing %q", line, want)
		}
	}
	empty := attachGroupLine(types.BgpAttachGroup{SetupBgpAttachRequest: types.SetupBgpAttachRequest{Rack: "rack2", Peer: "65102@fe80::1"}})
	if !strings.Contains(empty, "members: (none yet)") {
		t.Errorf("line %q should say no member is labelled yet", empty)
	}
}

func TestLabelFlagsValidation(t *testing.T) {
	c := &cmdSetupBgpAttach{flagLabelClear: true, flagLabelRack: "rack1"}
	if err := c.runLabel(nil, nil); err == nil || !strings.Contains(err.Error(), "either --rack or --clear") {
		t.Fatalf("got %v", err)
	}
	c = &cmdSetupBgpAttach{}
	if err := c.runLabel(nil, nil); err == nil || !strings.Contains(err.Error(), "--rack is required") {
		t.Fatalf("got %v", err)
	}
}

func TestRunRequiresRack(t *testing.T) {
	c := &cmdSetupBgpAttach{flagPeer: "65101@fe80::1"}
	if err := c.Run(nil, nil); err == nil || !strings.Contains(err.Error(), "--rack is required") {
		t.Fatalf("got %v", err)
	}
}

func TestPrintAttachResponseWarningsExitZero(t *testing.T) {
	err := printAttachResponse(types.SetupBgpAttachResponse{
		Success:  true,
		Message:  "bgp-attach group 65101@192.0.2.17 applied (2 member(s)), with 1 warning(s)",
		Nodes:    []types.BgpAttachNodePlan{{Member: "c1", Error: "not reached (x)"}},
		Warnings: []string{"c1: not reached (x)"},
	})
	if err != nil {
		t.Fatalf("warnings must keep exit 0, got %v", err)
	}
}
