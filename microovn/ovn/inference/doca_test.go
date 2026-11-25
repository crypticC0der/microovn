package inference

import "testing"

func TestAssessDocaReady(t *testing.T) {
	engines := []Engine{{Name: "ovs-doca", Devices: docaDevices()}}

	readiness := AssessDoca(engines, docaHardware())
	if !readiness.Capable {
		t.Fatal("expected a DOCA-capable host")
	}

	if !readiness.Ready {
		t.Fatalf("expected ready, got missing %v", readiness.Missing)
	}
}

func TestAssessDocaCapableButNotReady(t *testing.T) {
	engines := []Engine{{Name: "ovs-doca", Devices: docaDevices()}}

	hardware := docaHardware()
	hardware.HugepageKiBs = nil

	readiness := AssessDoca(engines, hardware)
	if !readiness.Capable {
		t.Fatal("expected a DOCA-capable host")
	}

	if readiness.Ready {
		t.Fatal("expected not ready without hugepages")
	}

	if len(readiness.Missing) == 0 {
		t.Fatal("expected missing prerequisites to be reported")
	}
}

func TestAssessDocaNotCapable(t *testing.T) {
	engines := []Engine{{Name: "ovs-doca", Devices: docaDevices()}}

	readiness := AssessDoca(engines, &Hardware{Architecture: "amd64"})
	if readiness.Capable {
		t.Fatal("expected not DOCA-capable without a ConnectX NIC")
	}

	if readiness.Ready {
		t.Fatal("expected not ready on a non-capable host")
	}

	if readiness.Summarise() != "" {
		t.Fatalf("expected no summary on a non-capable host, got %q", readiness.Summarise())
	}
}

func TestAssessDocaNoDocaEngine(t *testing.T) {
	readiness := AssessDoca([]Engine{{Name: "ovs"}}, docaHardware())
	if readiness.Capable {
		t.Fatal("expected not capable when no DOCA engine is present")
	}
}

func TestAssessDocaDpu(t *testing.T) {
	engines := []Engine{{Name: "ovs-doca-dpu", Devices: dpuDevices()}}

	readiness := AssessDoca(engines, dpuHardware())
	if !readiness.Capable {
		t.Fatal("expected a DOCA-capable DPU")
	}

	if !readiness.Ready {
		t.Fatalf("expected the DPU to be ready, got missing %v", readiness.Missing)
	}

	if !UseDocaDatapath("ovs-doca-dpu", readiness) {
		t.Fatal("expected the DPU engine to request the DOCA datapath")
	}
}

func TestDocaDpuRequires2MHugepages(t *testing.T) {
	engines := []Engine{
		{Name: "ovs-doca", Devices: docaDevices()},
		{Name: "ovs-doca-dpu", Devices: dpuDevices()},
	}

	// Arm64 DPU hardware with 2M hugepages reserved selects the DPU engine.
	engine, ok := findDocaEngine(engines, dpuHardware())
	if !ok {
		t.Fatal("expected a DOCA engine to match the DPU hardware")
	}

	if engine.Name != "ovs-doca-dpu" {
		t.Fatalf("expected the DPU engine, got %q", engine.Name)
	}

	// Without 2M hugepages the DPU engine is no longer ready.
	hardware := dpuHardware()
	hardware.HugepageKiBs = nil
	readiness := AssessDoca(engines, hardware)
	if readiness.Ready {
		t.Fatal("expected the DPU engine to be not-ready without 2M hugepages")
	}
}

func TestUseDocaDatapath(t *testing.T) {
	if !UseDocaDatapath("ovs-doca", DocaReadiness{Capable: true, Ready: true}) {
		t.Fatal("expected DOCA datapath for an active, ready DOCA engine")
	}

	if UseDocaDatapath("ovs-doca", DocaReadiness{Capable: true}) {
		t.Fatal("expected no DOCA datapath for a not-ready DOCA engine")
	}

	if UseDocaDatapath("ovs", DocaReadiness{Capable: true, Ready: true}) {
		t.Fatal("expected no DOCA datapath for the kernel engine")
	}
}

func TestDocaReadinessSummarise(t *testing.T) {
	ready := DocaReadiness{Capable: true, Ready: true}
	if got := ready.Summarise(); got != "ready" {
		t.Fatalf("expected %q, got %q", "ready", got)
	}

	notReady := DocaReadiness{Capable: true, Missing: []string{"1G hugepages"}}
	if got := notReady.Summarise(); got != "not ready (missing: 1G hugepages)" {
		t.Fatalf("got %q", got)
	}
}
