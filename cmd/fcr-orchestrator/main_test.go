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

func TestValidateEngineManifest_RejectsEmptyEngineName(t *testing.T) {
	m := manifest.EngineManifest{BuildFlags: []string{"fake_crypto"}}
	err := validateEngineManifest("lighthouse", "/tmp/fcr-x", m)
	if err == nil || !strings.Contains(err.Error(), "did not report engine_name") {
		t.Errorf("expected engine_name-required error, got %v", err)
	}
}

func TestValidateEngineManifest_RejectsMismatchedName(t *testing.T) {
	m := manifest.EngineManifest{EngineName: "teku", BuildFlags: []string{"fake_crypto"}}
	err := validateEngineManifest("lighthouse", "/tmp/fcr-x", m)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("expected name-mismatch error, got %v", err)
	}
}

func TestValidateEngineManifest_RejectsMissingRequiredFlag(t *testing.T) {
	m := manifest.EngineManifest{EngineName: "lighthouse", BuildFlags: []string{"unrelated"}}
	err := validateEngineManifest("lighthouse", "/tmp/fcr-x", m)
	if err == nil || !strings.Contains(err.Error(), `missing required build flag "fake_crypto"`) {
		t.Errorf("expected missing-build-flag error, got %v", err)
	}
}

func TestValidateEngineManifest_AcceptsMatching(t *testing.T) {
	m := manifest.EngineManifest{EngineName: "grandine", BuildFlags: []string{"fake_crypto", "extra"}}
	if err := validateEngineManifest("grandine", "/tmp/fcr-x", m); err != nil {
		t.Errorf("expected success, got %v", err)
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
