package main

import (
	"strings"
	"testing"

	"github.com/ethpandaops/fcr-simulator/pkg/manifest"
)

func TestEndToEndSmoke(t *testing.T) {
	t.Skip("TODO: wire a mock beacon node plus fixture ERA cache for an orchestrator end-to-end smoke test")
}

func validConfig(engine string) *config {
	return &config{
		Engine:                engine,
		EngineBinary:          "/tmp/fcr-x",
		Network:               "mainnet",
		StartEpoch:            1,
		EndEpoch:              2,
		Parallel:              1,
		BeaconNodeURL:         "http://localhost:5052",
		EraURL:                "https://example.com",
		CacheDir:              "/tmp",
		Output:                "out.csv",
		OutputFormat:          "csv",
		AttestationSourceMode: "next-non-missed",
		LookaheadCap:          4,
		HTTPListen:            "127.0.0.1:0",
	}
}

func TestValidateConfig_AcceptsAllSupportedEngines(t *testing.T) {
	for engine := range supportedEngines {
		if err := validateConfig(validConfig(engine), true, true); err != nil {
			t.Errorf("engine %q rejected: %v", engine, err)
		}
	}
}

func TestValidateConfig_RejectsUnknownEngine(t *testing.T) {
	err := validateConfig(validConfig("notreal"), true, true)
	if err == nil {
		t.Fatal("expected error for unknown engine")
	}
	if !strings.Contains(err.Error(), "supported values are") {
		t.Errorf("error should list supported values: %v", err)
	}
}

func TestSupportedEngineList_IsSorted(t *testing.T) {
	got := supportedEngineList()
	want := "grandine, lighthouse, lodestar, nimbus, prysm, teku"
	if got != want {
		t.Errorf("supportedEngineList()=%q want=%q", got, want)
	}
}

func TestEngineHasBuildFlag(t *testing.T) {
	m := manifest.EngineManifest{BuildFlags: []string{"fake_crypto", "noop_el"}}
	if !engineHasBuildFlag(m, "fake_crypto") {
		t.Error("expected fake_crypto to be present")
	}
	if engineHasBuildFlag(m, "missing") {
		t.Error("did not expect missing flag")
	}
}

func TestSupportedEngines_AllRequireFakeCrypto(t *testing.T) {
	for engine, spec := range supportedEngines {
		found := false
		for _, f := range spec.RequiredBuildFlags {
			if f == "fake_crypto" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("engine %q must require fake_crypto until BLS replay bypass is standardized", engine)
		}
	}
}
