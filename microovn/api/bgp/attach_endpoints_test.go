package bgpapi

import (
	"strings"
	"testing"

	"github.com/canonical/microovn/microovn/api/types"
)

func TestSummariseWarningsKeepsSuccess(t *testing.T) {
	resp := types.SetupBgpAttachResponse{Nodes: []types.BgpAttachNodePlan{
		{Member: "c0", Attached: true},
		{Member: "c1", Error: "not reached (connection refused); it will converge on its next reconcile tick"},
		{Member: "c2", Error: `no router port matching "*br-bgp" on a router pinned to chassis "c2"`},
	}}
	summariseWarnings(&resp, "bgp-attach group 65101@192.0.2.17 applied (3 member(s))")

	if !resp.Success {
		t.Fatal("per-member problems must not fail a request that was carried out")
	}
	if len(resp.Errors) != 0 {
		t.Fatalf("errors %v, want none", resp.Errors)
	}
	if len(resp.Warnings) != 2 || !strings.HasPrefix(resp.Warnings[0], "c1: not reached") || !strings.HasPrefix(resp.Warnings[1], "c2: no router port") {
		t.Fatalf("warnings %v", resp.Warnings)
	}
	if !strings.HasSuffix(resp.Message, "with 2 warning(s)") {
		t.Fatalf("message %q", resp.Message)
	}
}

func TestSummariseWarningsAllGood(t *testing.T) {
	resp := types.SetupBgpAttachResponse{Nodes: []types.BgpAttachNodePlan{{Member: "c0", Attached: true}}}
	summariseWarnings(&resp, "applied")
	if !resp.Success || resp.Message != "applied" || len(resp.Warnings) != 0 {
		t.Fatalf("got %+v", resp)
	}
}

func TestNbWriteErrorStillFails(t *testing.T) {
	resp := types.SetupBgpAttachResponse{Nodes: []types.BgpAttachNodePlan{{Member: "c1", Error: "not reached (x)"}}}
	summariseWarnings(&resp, "applied")
	addNbErrors(&resp, []string{"failed to write attach keys on 3 router port(s): timeout"})

	if resp.Success {
		t.Fatal("a failed router-port write must fail the request")
	}
	if len(resp.Errors) != 1 || len(resp.Warnings) != 1 {
		t.Fatalf("errors %v warnings %v", resp.Errors, resp.Warnings)
	}
	if !strings.Contains(resp.Message, "rerun the call") {
		t.Fatalf("message %q", resp.Message)
	}
}
