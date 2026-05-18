package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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

func validArgs(engine string) []string {
	return []string{
		"--engine", engine,
		"--network", "mainnet",
		"--start-epoch", "1",
		"--end-epoch", "2",
		"--beacon-node-url", "http://localhost:5052",
	}
}

func TestParseConfig_DefaultsEngineBinaryByEngine(t *testing.T) {
	t.Setenv("FCR_ENGINE_BINARY", "")

	cfg, printVersion, err := parseConfig(validArgs("lighthouse"), ioDiscard{})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if printVersion {
		t.Fatal("did not expect version mode")
	}
	if cfg.EngineBinary != "./results/fcr-lighthouse" {
		t.Fatalf("EngineBinary=%q want %q", cfg.EngineBinary, "./results/fcr-lighthouse")
	}
	if cfg.engineBinarySource != engineBinarySourceDefault {
		t.Fatalf("engineBinarySource=%v want default", cfg.engineBinarySource)
	}
}

func TestParseConfig_EngineBinaryEnvOverridesDefault(t *testing.T) {
	t.Setenv("FCR_ENGINE_BINARY", "/env/fcr-lighthouse")

	cfg, _, err := parseConfig(validArgs("lighthouse"), ioDiscard{})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if cfg.EngineBinary != "/env/fcr-lighthouse" {
		t.Fatalf("EngineBinary=%q want env path", cfg.EngineBinary)
	}
	if cfg.engineBinarySource != engineBinarySourceEnv {
		t.Fatalf("engineBinarySource=%v want env", cfg.engineBinarySource)
	}
}

func TestParseConfig_EngineBinaryFlagOverridesEnv(t *testing.T) {
	t.Setenv("FCR_ENGINE_BINARY", "/env/fcr-lighthouse")

	args := append(validArgs("lighthouse"), "--engine-binary", "/flag/fcr-lighthouse")
	cfg, _, err := parseConfig(args, ioDiscard{})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if cfg.EngineBinary != "/flag/fcr-lighthouse" {
		t.Fatalf("EngineBinary=%q want flag path", cfg.EngineBinary)
	}
	if cfg.engineBinarySource != engineBinarySourceFlag {
		t.Fatalf("engineBinarySource=%v want flag", cfg.engineBinarySource)
	}
}

func TestPrepareEngineBinary_DefaultUsesExistingBinary(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("results", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("results", "fcr-lighthouse"), []byte("#!/usr/bin/env bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config{
		Engine:             "lighthouse",
		EngineBinary:       defaultEngineBinaryPath("lighthouse"),
		engineBinarySource: engineBinarySourceDefault,
	}
	var stderr bytes.Buffer
	if err := prepareEngineBinary(context.Background(), &cfg, &stderr); err != nil {
		t.Fatalf("prepareEngineBinary returned error: %v", err)
	}

	want, err := filepath.Abs(filepath.Join("results", "fcr-lighthouse"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EngineBinary != want {
		t.Fatalf("EngineBinary=%q want %q", cfg.EngineBinary, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q want empty", stderr.String())
	}
}

func TestPrepareEngineBinary_DefaultRunsBuildScriptWhenMissing(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(filepath.Join("engines", "lighthouse"), 0o755); err != nil {
		t.Fatal(err)
	}
	buildScript := filepath.Join("engines", "lighthouse", "build.sh")
	script := `#!/usr/bin/env bash
set -euo pipefail
echo "building lighthouse" >&2
mkdir -p results
printf '#!/usr/bin/env bash\n' > results/fcr-lighthouse
chmod +x results/fcr-lighthouse
`
	if err := os.WriteFile(buildScript, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config{
		Engine:             "lighthouse",
		EngineBinary:       defaultEngineBinaryPath("lighthouse"),
		engineBinarySource: engineBinarySourceDefault,
	}
	var stderr bytes.Buffer
	if err := prepareEngineBinary(context.Background(), &cfg, &stderr); err != nil {
		t.Fatalf("prepareEngineBinary returned error: %v", err)
	}

	want, err := filepath.Abs(filepath.Join("results", "fcr-lighthouse"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EngineBinary != want {
		t.Fatalf("EngineBinary=%q want %q", cfg.EngineBinary, want)
	}
	gotStderr := stderr.String()
	if !strings.Contains(gotStderr, "binary not found, running engines/lighthouse/build.sh...") {
		t.Fatalf("stderr=%q missing auto-build notice", gotStderr)
	}
	if !strings.Contains(gotStderr, "building lighthouse") {
		t.Fatalf("stderr=%q missing build output", gotStderr)
	}
}

func TestPrepareEngineBinary_DefaultMissingWithoutBuildScriptReturnsClearError(t *testing.T) {
	t.Chdir(t.TempDir())

	cfg := config{
		Engine:             "lighthouse",
		EngineBinary:       defaultEngineBinaryPath("lighthouse"),
		engineBinarySource: engineBinarySourceDefault,
	}
	var stderr bytes.Buffer
	err := prepareEngineBinary(context.Background(), &cfg, &stderr)
	if err == nil {
		t.Fatal("expected error")
	}
	want := "engine binary not found at ./results/fcr-lighthouse; build it via engines/lighthouse/build.sh or pass --engine-binary <path>"
	if err.Error() != want {
		t.Fatalf("error=%q want %q", err.Error(), want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q want empty", stderr.String())
	}
}

func TestPrepareEngineBinary_ExplicitBypassesAutoBuild(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(filepath.Join("engines", "lighthouse"), 0o755); err != nil {
		t.Fatal(err)
	}
	buildScript := filepath.Join("engines", "lighthouse", "build.sh")
	if err := os.WriteFile(buildScript, []byte("#!/usr/bin/env bash\ntouch build-ran\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config{
		Engine:             "lighthouse",
		EngineBinary:       "./custom/fcr-lighthouse",
		engineBinarySource: engineBinarySourceFlag,
	}
	var stderr bytes.Buffer
	err := prepareEngineBinary(context.Background(), &cfg, &stderr)
	if err == nil {
		t.Fatal("expected error")
	}
	if _, statErr := os.Stat("build-ran"); !os.IsNotExist(statErr) {
		t.Fatalf("build script should not have run, stat err=%v", statErr)
	}
	if strings.Contains(stderr.String(), "binary not found, running") {
		t.Fatalf("stderr=%q should not contain auto-build notice", stderr.String())
	}
}

func TestPrepareEngineBinary_EnvBypassesAutoBuild(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(filepath.Join("engines", "lighthouse"), 0o755); err != nil {
		t.Fatal(err)
	}
	buildScript := filepath.Join("engines", "lighthouse", "build.sh")
	if err := os.WriteFile(buildScript, []byte("#!/usr/bin/env bash\ntouch build-ran\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config{
		Engine:             "lighthouse",
		EngineBinary:       "./env/fcr-lighthouse",
		engineBinarySource: engineBinarySourceEnv,
	}
	var stderr bytes.Buffer
	err := prepareEngineBinary(context.Background(), &cfg, &stderr)
	if err == nil {
		t.Fatal("expected error")
	}
	if _, statErr := os.Stat("build-ran"); !os.IsNotExist(statErr) {
		t.Fatalf("build script should not have run, stat err=%v", statErr)
	}
	if strings.Contains(stderr.String(), "binary not found, running") {
		t.Fatalf("stderr=%q should not contain auto-build notice", stderr.String())
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) {
	return len(p), nil
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
