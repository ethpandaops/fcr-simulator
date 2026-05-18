package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethpandaops/fcr-simulator/pkg/attplan"
	"github.com/ethpandaops/fcr-simulator/pkg/beaconapi"
	"github.com/ethpandaops/fcr-simulator/pkg/beaconfetch"
	"github.com/ethpandaops/fcr-simulator/pkg/chunk"
	"github.com/ethpandaops/fcr-simulator/pkg/era"
	"github.com/ethpandaops/fcr-simulator/pkg/manifest"
	"github.com/ethpandaops/fcr-simulator/pkg/merge"
	"github.com/ethpandaops/fcr-simulator/pkg/schema"
)

const (
	defaultWarmupEpochs          = uint64(10)
	defaultParallel              = 1
	defaultEraURL                = "https://mainnet.era.nimbus.team"
	defaultCacheDir              = "~/.cache/fcr-simulator"
	defaultOutput                = "results.csv"
	defaultOutputFormat          = "both"
	defaultByzantineThreshold    = uint64(25)
	defaultAttestationSourceMode = "next-non-missed"
	defaultLookaheadCap          = uint64(4)
	defaultHTTPListen            = "127.0.0.1:0"
)

var version = "dev"

type engineSpec struct {
	RequiredBuildFlags []string
}

var supportedEngines = map[string]engineSpec{
	"lighthouse": {RequiredBuildFlags: []string{"fake_crypto"}},
	"teku":       {RequiredBuildFlags: []string{"fake_crypto"}},
	"lodestar":   {RequiredBuildFlags: []string{"fake_crypto"}},
	"nimbus":     {RequiredBuildFlags: []string{"fake_crypto"}},
	"prysm":      {RequiredBuildFlags: []string{"fake_crypto"}},
	"grandine":   {RequiredBuildFlags: []string{"fake_crypto"}},
}

func supportedEngineList() string {
	names := make([]string, 0, len(supportedEngines))
	for name := range supportedEngines {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

type config struct {
	Engine                string
	EngineBinary          string
	Network               string
	StartEpoch            uint64
	EndEpoch              uint64
	WarmupEpochs          uint64
	Parallel              int
	BeaconNodeURL         string
	EraURL                string
	CacheDir              string
	Output                string
	OutputFormat          string
	ByzantineThreshold    uint64
	AttestationSourceMode string
	LookaheadCap          uint64
	HTTPListen            string
	KeepCache             bool
	PrepOnly              bool
}

type requiredUint64Flag struct {
	value uint64
	set   bool
}

func (f *requiredUint64Flag) String() string {
	return strconv.FormatUint(f.value, 10)
}

func (f *requiredUint64Flag) Set(value string) error {
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return err
	}
	f.value = parsed
	f.set = true
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, printVersion, err := parseConfig(args, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if printVersion {
		fmt.Fprintf(stdout, "fcr-orchestrator %s\n", simulatorVersion())
		return 0
	}

	exitCode, err := execute(ctx, cfg, stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitCode
	}
	return exitCode
}

func parseConfig(args []string, output io.Writer) (config, bool, error) {
	var cfg config
	var startEpoch requiredUint64Flag
	var endEpoch requiredUint64Flag
	var printVersion bool

	fs := flag.NewFlagSet("fcr-orchestrator", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&cfg.Engine, "engine", "", "engine name (one of: "+supportedEngineList()+")")
	fs.StringVar(&cfg.EngineBinary, "engine-binary", os.Getenv("FCR_ENGINE_BINARY"), "path to engine binary (env: FCR_ENGINE_BINARY)")
	fs.StringVar(&cfg.Network, "network", "", "network name (V1 supports mainnet)")
	fs.Var(&startEpoch, "start-epoch", "first epoch, inclusive")
	fs.Var(&endEpoch, "end-epoch", "end epoch, exclusive")
	fs.Uint64Var(&cfg.WarmupEpochs, "warmup-epochs", defaultWarmupEpochs, "warmup epochs per worker")
	fs.IntVar(&cfg.Parallel, "parallel", defaultParallel, "number of workers")
	fs.StringVar(&cfg.BeaconNodeURL, "beacon-node-url", os.Getenv("BN_URL"), "real beacon node URL (env: BN_URL)")
	fs.StringVar(&cfg.EraURL, "era-url", defaultEraURL, "ERA file base URL")
	fs.StringVar(&cfg.CacheDir, "cache-dir", defaultCacheDir, "cache directory")
	fs.StringVar(&cfg.Output, "output", defaultOutput, "output path")
	fs.StringVar(&cfg.OutputFormat, "output-format", defaultOutputFormat, "csv, jsonl, or both")
	fs.Uint64Var(&cfg.ByzantineThreshold, "byzantine-threshold", defaultByzantineThreshold, "FCR byzantine threshold percent")
	fs.StringVar(&cfg.AttestationSourceMode, "attestation-source-mode", defaultAttestationSourceMode, "next-non-missed or strict-source-block-k-minus-1")
	fs.Uint64Var(&cfg.LookaheadCap, "lookahead-cap", defaultLookaheadCap, "attestation lookahead cap")
	fs.StringVar(&cfg.HTTPListen, "http-listen", defaultHTTPListen, "local HTTP listen address")
	fs.BoolVar(&cfg.KeepCache, "keep-cache", false, "keep intermediate cache after run")
	fs.BoolVar(&cfg.PrepOnly, "prep-only", false, "download ERA files and checkpoint state then exit (no engine run)")
	fs.BoolVar(&printVersion, "version", false, "print orchestrator version and exit")

	if err := fs.Parse(args); err != nil {
		return config{}, false, err
	}
	if printVersion {
		return config{}, true, nil
	}
	if fs.NArg() != 0 {
		return config{}, false, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}

	cfg.StartEpoch = startEpoch.value
	cfg.EndEpoch = endEpoch.value
	if err := validateConfig(&cfg, startEpoch.set, endEpoch.set); err != nil {
		return config{}, false, err
	}

	expandedCacheDir, err := expandPath(cfg.CacheDir)
	if err != nil {
		return config{}, false, err
	}
	cfg.CacheDir = expandedCacheDir

	engineBinary, err := resolveExecutable(cfg.EngineBinary)
	if err != nil {
		return config{}, false, err
	}
	cfg.EngineBinary = engineBinary

	cfg.BeaconNodeURL = strings.TrimRight(strings.TrimSpace(cfg.BeaconNodeURL), "/")
	cfg.EraURL = strings.TrimRight(strings.TrimSpace(cfg.EraURL), "/")
	cfg.OutputFormat = strings.ToLower(strings.TrimSpace(cfg.OutputFormat))
	cfg.AttestationSourceMode = strings.TrimSpace(cfg.AttestationSourceMode)
	if cfg.AttestationSourceMode == "strict-source-block-k-minus-1" {
		cfg.LookaheadCap = 0
	}

	return cfg, false, nil
}

func validateConfig(cfg *config, startSet, endSet bool) error {
	if cfg.Engine == "" {
		return fmt.Errorf("--engine is required")
	}
	if _, ok := supportedEngines[cfg.Engine]; !ok {
		return fmt.Errorf("--engine=%q is not supported; supported values are %s", cfg.Engine, supportedEngineList())
	}
	if cfg.EngineBinary == "" {
		return fmt.Errorf("--engine-binary is required")
	}
	if cfg.Network == "" {
		return fmt.Errorf("--network is required")
	}
	if cfg.Network != "mainnet" {
		return fmt.Errorf("--network=%q is not supported in V1; supported value is %q", cfg.Network, "mainnet")
	}
	if !startSet {
		return fmt.Errorf("--start-epoch is required")
	}
	if !endSet {
		return fmt.Errorf("--end-epoch is required")
	}
	if cfg.StartEpoch >= cfg.EndEpoch {
		return fmt.Errorf("--start-epoch (%d) must be less than --end-epoch (%d)", cfg.StartEpoch, cfg.EndEpoch)
	}
	if cfg.Parallel <= 0 {
		return fmt.Errorf("--parallel must be greater than zero")
	}
	if strings.TrimSpace(cfg.BeaconNodeURL) == "" {
		return fmt.Errorf("--beacon-node-url is required")
	}
	if err := validateHTTPURL(cfg.BeaconNodeURL, "--beacon-node-url"); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.EraURL) == "" {
		return fmt.Errorf("--era-url is required")
	}
	if err := validateHTTPURL(cfg.EraURL, "--era-url"); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.CacheDir) == "" {
		return fmt.Errorf("--cache-dir is required")
	}
	if strings.TrimSpace(cfg.Output) == "" {
		return fmt.Errorf("--output is required")
	}
	switch strings.ToLower(strings.TrimSpace(cfg.OutputFormat)) {
	case "csv", "jsonl", "both":
	default:
		return fmt.Errorf("--output-format must be one of csv, jsonl, both")
	}
	switch cfg.AttestationSourceMode {
	case "next-non-missed":
		if cfg.LookaheadCap == 0 {
			return fmt.Errorf("--lookahead-cap must be greater than zero for next-non-missed mode")
		}
	case "strict-source-block-k-minus-1":
	default:
		return fmt.Errorf("--attestation-source-mode must be next-non-missed or strict-source-block-k-minus-1")
	}
	if strings.TrimSpace(cfg.HTTPListen) == "" {
		return fmt.Errorf("--http-listen is required")
	}
	return nil
}

func execute(ctx context.Context, cfg config, stdout io.Writer) (int, error) {
	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		return 1, fmt.Errorf("create cache directory %q: %w", cfg.CacheDir, err)
	}

	fmt.Fprintf(stdout, "capturing engine manifest from %s\n", cfg.EngineBinary)
	engineManifest, err := captureEngineManifest(ctx, cfg.EngineBinary)
	if err != nil {
		return 1, err
	}
	if err := validateEngineManifest(cfg.Engine, cfg.EngineBinary, engineManifest); err != nil {
		return 1, err
	}

	chunks := chunk.Split(cfg.StartEpoch, cfg.EndEpoch, cfg.WarmupEpochs, cfg.Parallel)
	if len(chunks) == 0 {
		return 1, fmt.Errorf("no chunks generated")
	}
	activeChunks := filterActiveChunks(chunks)
	if len(activeChunks) == 0 {
		return 1, fmt.Errorf("no non-empty chunks generated")
	}

	fmt.Fprintf(stdout, "pre-downloading ERA files for slots %d through %d\n", minWarmupSlot(activeChunks), maxEndSlot(activeChunks))
	downloader, err := era.NewDownloader(cfg.EraURL, cfg.CacheDir)
	if err != nil {
		return 1, err
	}
	if err := downloader.PreDownload(minWarmupSlot(activeChunks), maxEndSlot(activeChunks)); err != nil {
		return 1, fmt.Errorf("pre-download ERA files: %w", err)
	}

	eraReader, err := era.New(downloader.CacheDir())
	if err != nil {
		return 1, err
	}

	fetcher, err := beaconfetch.New(cfg.BeaconNodeURL, cfg.CacheDir)
	if err != nil {
		return 1, err
	}

	workerInfos, checkpointStates, checkpointBlocks, err := prepareWorkers(cfg, fetcher, chunks, stdout)
	if err != nil {
		return 1, err
	}

	fmt.Fprintln(stdout, "fetching genesis state")
	if _, err := fetcher.FetchGenesisStateSSZ(); err != nil {
		return 1, fmt.Errorf("fetch genesis state: %w", err)
	}

	if cfg.PrepOnly {
		_ = eraReader
		fmt.Fprintf(stdout, "prep complete: %d checkpoint state(s) cached for %d worker(s)\n",
			len(checkpointStates), len(workerInfos))
		return 0, nil
	}

	server, serverURL, shutdown, err := startBeaconAPIServer(cfg, eraReader, fetcher, checkpointBlocks)
	if err != nil {
		return 1, err
	}
	defer shutdown()
	_ = server
	fmt.Fprintf(stdout, "local beacon API listening on %s\n", serverURL)

	fmt.Fprintf(stdout, "starting %d engine worker(s)\n", len(activeChunks))
	results := runEngineWorkers(ctx, cfg, workerInfos, serverURL, stdout)
	hadWorkerFailure := false
	for _, result := range results {
		if result.Err != nil {
			hadWorkerFailure = true
			if result.ExitCode >= 0 {
				fmt.Fprintf(stdout, "worker %d failed with exit code %d: %v\n", result.Index, result.ExitCode, result.Err)
			} else {
				fmt.Fprintf(stdout, "worker %d failed: %v\n", result.Index, result.Err)
			}
		}
	}

	paths := resolveOutputPaths(cfg.Output, cfg.OutputFormat)
	mergePaths, err := collectMergePaths(workerInfos, results)
	if err != nil {
		return 1, err
	}

	fmt.Fprintln(stdout, "validating and merging worker JSONL")
	expectedSlots := make([]uint64, 0)
	for _, c := range activeChunks {
		recordStart := c.StartSlot
		if c.WarmupStartSlot+1 > recordStart {
			recordStart = c.WarmupStartSlot + 1
		}
		for s := recordStart; s < c.EndSlot; s++ {
			expectedSlots = append(expectedSlots, s)
		}
	}
	stats, err := merge.MergeAndWrite(mergePaths, paths.JSONL, paths.CSV, schema.OrchestratorMetadata{
		EngineName:            engineManifest.EngineName,
		EngineVersion:         engineManifest.EngineVersion,
		EngineCommit:          engineManifest.EngineCommit,
		AttestationSourceMode: cfg.AttestationSourceMode,
		LookaheadCap:          cfg.LookaheadCap,
	}, expectedSlots)
	if err != nil {
		return 1, fmt.Errorf("merge worker outputs: %w", err)
	}

	if err := writeRunManifest(cfg, engineManifest, downloader.CacheDir(), checkpointStates, paths, stats); err != nil {
		return 1, err
	}
	fmt.Fprintf(stdout, "wrote manifest %s\n", paths.Manifest)

	if !cfg.KeepCache && !hadWorkerFailure {
		cleanupWorkerCache(filepath.Join(cfg.CacheDir, "workers"), stdout)
	}

	if hadWorkerFailure {
		return 1, nil
	}
	return 0, nil
}

func engineHasBuildFlag(m manifest.EngineManifest, flag string) bool {
	for _, f := range m.BuildFlags {
		if f == flag {
			return true
		}
	}
	return false
}

func validateEngineManifest(engine, binary string, m manifest.EngineManifest) error {
	if m.EngineName == "" {
		return fmt.Errorf("engine manifest from %s did not report engine_name", binary)
	}
	if m.EngineName != engine {
		return fmt.Errorf("engine manifest name %q does not match --engine=%q", m.EngineName, engine)
	}
	for _, flag := range supportedEngines[engine].RequiredBuildFlags {
		if !engineHasBuildFlag(m, flag) {
			return fmt.Errorf("engine %s is missing required build flag %q (got build_flags=%v)", binary, flag, m.BuildFlags)
		}
	}
	return nil
}

func captureEngineManifest(ctx context.Context, engineBinary string) (manifest.EngineManifest, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, engineBinary, "--manifest-json")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return manifest.EngineManifest{}, fmt.Errorf("capture engine manifest: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var engineManifest manifest.EngineManifest
	if err := json.Unmarshal(stdout.Bytes(), &engineManifest); err != nil {
		return manifest.EngineManifest{}, fmt.Errorf("decode engine manifest JSON: %w", err)
	}

	if engineManifest.BuildFlags == nil {
		engineManifest.BuildFlags = []string{}
	}
	return engineManifest, nil
}

type workerInfo struct {
	Chunk                 chunk.Chunk
	OutputPath            string
	ActualWarmupStartSlot uint64
	Skipped               bool
}

func prepareWorkers(cfg config, fetcher *beaconfetch.Fetcher, chunks []chunk.Chunk, stdout io.Writer) ([]workerInfo, []manifest.CheckpointState, map[[32]byte][]byte, error) {
	workersDir := filepath.Join(cfg.CacheDir, "workers")
	if err := os.MkdirAll(workersDir, 0o755); err != nil {
		return nil, nil, nil, fmt.Errorf("create workers directory %q: %w", workersDir, err)
	}

	infos := make([]workerInfo, 0, len(chunks))
	checkpointStates := make([]manifest.CheckpointState, 0, len(chunks))
	checkpointBlocks := make(map[[32]byte][]byte)

	for _, c := range chunks {
		outputPath := filepath.Join(workersDir, fmt.Sprintf("worker-%d.jsonl", c.Index))
		if err := os.Remove(outputPath); err != nil && !os.IsNotExist(err) {
			return nil, nil, nil, fmt.Errorf("remove stale worker output %q: %w", outputPath, err)
		}

		info := workerInfo{Chunk: c, OutputPath: outputPath}
		if c.StartEpoch == c.EndEpoch {
			if err := os.WriteFile(outputPath, nil, 0o644); err != nil {
				return nil, nil, nil, fmt.Errorf("create empty worker output %q: %w", outputPath, err)
			}
			info.Skipped = true
			infos = append(infos, info)
			continue
		}

		fmt.Fprintf(stdout, "fetching checkpoint state for worker %d at warmup slot %d\n", c.Index, c.WarmupStartSlot)
		actualSlot, stateSSZ, err := fetcher.FetchCheckpointAtWarmupSlot(c.WarmupStartSlot)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("worker %d checkpoint state: %w", c.Index, err)
		}

		root, err := fetcher.CheckpointBlockRootAtSlot(actualSlot)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("worker %d checkpoint block root at slot %d: %w", c.Index, actualSlot, err)
		}

		blockSSZ, err := fetcher.FetchBlockSSZByRoot(root)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("worker %d checkpoint block by root at slot %d: %w", c.Index, actualSlot, err)
		}

		checkpointBlocks[root] = blockSSZ
		checkpointStates = append(checkpointStates, manifest.CheckpointState{
			Worker: c.Index,
			Slot:   actualSlot,
			SHA256: manifest.SHA256Bytes(stateSSZ),
		})

		info.ActualWarmupStartSlot = actualSlot
		infos = append(infos, info)
	}

	return infos, checkpointStates, checkpointBlocks, nil
}

func startBeaconAPIServer(cfg config, eraReader *era.Reader, fetcher *beaconfetch.Fetcher, checkpointBlocks map[[32]byte][]byte) (*http.Server, string, func(), error) {
	mode, err := parseAttplanMode(cfg.AttestationSourceMode)
	if err != nil {
		return nil, "", nil, err
	}

	backend := beaconapi.NewRealBackend(beaconapi.RealBackendConfig{
		EraReader:              eraReader,
		Fetcher:                fetcher,
		GenesisInfo:            mainnetGenesisInfo(),
		ForkSchedule:           beaconapi.ForkSchedule{SlotFork: beaconapi.MainnetForkAtSlot},
		Mode:                   mode,
		LookaheadCap:           cfg.LookaheadCap,
		CheckpointBlocksByRoot: checkpointBlocks,
	})

	server := &http.Server{
		Handler: beaconapi.NewServer(backend).Handler(),
	}
	listener, err := net.Listen("tcp", cfg.HTTPListen)
	if err != nil {
		return nil, "", nil, fmt.Errorf("start HTTP listener on %q: %w", cfg.HTTPListen, err)
	}

	serverErr := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
		}
		<-serverErr
	}

	return server, listenerHTTPURL(listener.Addr()), shutdown, nil
}

type workerResult struct {
	Index    int
	Skipped  bool
	ExitCode int
	Err      error
}

func runEngineWorkers(ctx context.Context, cfg config, workers []workerInfo, beaconNodeURL string, stdout io.Writer) []workerResult {
	results := make([]workerResult, len(workers))
	var wg sync.WaitGroup

	for i, worker := range workers {
		i := i
		worker := worker
		results[i] = workerResult{Index: worker.Chunk.Index, ExitCode: 0, Skipped: worker.Skipped}
		if worker.Skipped {
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = runEngineWorker(ctx, cfg, worker, beaconNodeURL, stdout)
		}()
	}

	wg.Wait()
	return results
}

func runEngineWorker(ctx context.Context, cfg config, worker workerInfo, beaconNodeURL string, stdout io.Writer) workerResult {
	args := []string{
		"--beacon-node-url", beaconNodeURL,
		"--start-slot", strconv.FormatUint(worker.Chunk.StartSlot, 10),
		"--end-slot", strconv.FormatUint(worker.Chunk.EndSlot, 10),
		"--warmup-start-slot", strconv.FormatUint(worker.ActualWarmupStartSlot, 10),
		"--network", cfg.Network,
		"--byzantine-threshold", strconv.FormatUint(cfg.ByzantineThreshold, 10),
		"--attestation-source-mode", cfg.AttestationSourceMode,
		"--lookahead-cap", strconv.FormatUint(cfg.LookaheadCap, 10),
		"--output", worker.OutputPath,
	}

	fmt.Fprintf(stdout, "worker %d running slots [%d, %d)\n", worker.Chunk.Index, worker.Chunk.StartSlot, worker.Chunk.EndSlot)
	cmd := exec.CommandContext(ctx, cfg.EngineBinary, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stdout

	if err := cmd.Run(); err != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		return workerResult{Index: worker.Chunk.Index, ExitCode: exitCode, Err: err}
	}

	return workerResult{Index: worker.Chunk.Index, ExitCode: 0}
}

func collectMergePaths(workers []workerInfo, results []workerResult) ([]string, error) {
	successByIndex := make(map[int]bool, len(results))
	for _, result := range results {
		successByIndex[result.Index] = result.Err == nil
	}

	paths := make([]string, 0, len(workers))
	for _, worker := range workers {
		if _, err := os.Stat(worker.OutputPath); err == nil {
			paths = append(paths, worker.OutputPath)
			continue
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat worker output %q: %w", worker.OutputPath, err)
		}

		if successByIndex[worker.Chunk.Index] {
			return nil, fmt.Errorf("worker %d succeeded but did not write %q", worker.Chunk.Index, worker.OutputPath)
		}
	}

	return paths, nil
}

type outputPaths struct {
	JSONL    string
	CSV      string
	Manifest string
}

func resolveOutputPaths(output, format string) outputPaths {
	format = strings.ToLower(format)
	ext := strings.ToLower(filepath.Ext(output))
	trimmed := strings.TrimSuffix(output, filepath.Ext(output))

	var paths outputPaths
	switch format {
	case "csv":
		paths.CSV = output
	case "jsonl":
		paths.JSONL = output
	case "both":
		switch ext {
		case ".csv":
			paths.CSV = output
			paths.JSONL = trimmed + ".jsonl"
		case ".jsonl":
			paths.JSONL = output
			paths.CSV = trimmed + ".csv"
		default:
			paths.CSV = output + ".csv"
			paths.JSONL = output + ".jsonl"
		}
	}

	manifestBase := output
	if paths.CSV != "" {
		manifestBase = paths.CSV
	} else if paths.JSONL != "" {
		manifestBase = paths.JSONL
	}
	paths.Manifest = strings.TrimSuffix(manifestBase, filepath.Ext(manifestBase)) + ".manifest.json"
	return paths
}

func writeRunManifest(cfg config, engineManifest manifest.EngineManifest, eraCacheDir string, checkpointStates []manifest.CheckpointState, paths outputPaths, stats merge.Stats) error {
	eraFiles, err := manifest.CollectEraFiles(eraCacheDir, cfg.EraURL)
	if err != nil {
		return fmt.Errorf("collect ERA file hashes: %w", err)
	}

	var jsonlSHA string
	if paths.JSONL != "" {
		jsonlSHA, err = manifest.SHA256File(paths.JSONL)
		if err != nil {
			return err
		}
	}

	var csvSHA string
	if paths.CSV != "" {
		csvSHA, err = manifest.SHA256File(paths.CSV)
		if err != nil {
			return err
		}
	}

	m := manifest.RunManifest{
		SchemaVersion:       schema.SchemaVersion,
		FCRSimulatorVersion: simulatorVersion(),
		RanAt:               time.Now().UTC().Format(time.RFC3339),
		Config: manifest.Config{
			Engine:                cfg.Engine,
			Network:               cfg.Network,
			StartEpoch:            cfg.StartEpoch,
			EndEpoch:              cfg.EndEpoch,
			WarmupEpochs:          cfg.WarmupEpochs,
			Parallel:              cfg.Parallel,
			AttestationSourceMode: cfg.AttestationSourceMode,
			LookaheadCap:          cfg.LookaheadCap,
			ByzantineThreshold:    cfg.ByzantineThreshold,
			BeaconNodeURL:         cfg.BeaconNodeURL,
			EraURL:                cfg.EraURL,
		},
		EngineManifest: engineManifest,
		Inputs: manifest.Inputs{
			EraFiles:         eraFiles,
			CheckpointStates: checkpointStates,
		},
		Outputs: manifest.Outputs{
			ResultsJSONLSHA256: jsonlSHA,
			ResultsCSVSHA256:   csvSHA,
			TotalSlots:         stats.TotalSlots,
			FastConfirmedCount: stats.FastConfirmedCount,
		},
	}

	return manifest.Write(paths.Manifest, m)
}

func parseAttplanMode(mode string) (attplan.Mode, error) {
	switch mode {
	case "next-non-missed":
		return attplan.ModeNextNonMissed, nil
	case "strict-source-block-k-minus-1":
		return attplan.ModeStrictKMinus1, nil
	default:
		return 0, fmt.Errorf("unsupported attestation source mode %q", mode)
	}
}

func mainnetGenesisInfo() beaconapi.GenesisInfo {
	return beaconapi.GenesisInfo{
		GenesisTime:           1606824023,
		GenesisValidatorsRoot: "0x4b363db94e286120d76eb905340fdd4e54bfe9f06bf33ff6cf5ad27f511bfe95",
		GenesisForkVersion:    "0x00000000",
	}
}

func filterActiveChunks(chunks []chunk.Chunk) []chunk.Chunk {
	active := make([]chunk.Chunk, 0, len(chunks))
	for _, c := range chunks {
		if c.StartEpoch < c.EndEpoch {
			active = append(active, c)
		}
	}
	return active
}

func minWarmupSlot(chunks []chunk.Chunk) uint64 {
	min := chunks[0].WarmupStartSlot
	for _, c := range chunks[1:] {
		if c.WarmupStartSlot < min {
			min = c.WarmupStartSlot
		}
	}
	return min
}

func maxEndSlot(chunks []chunk.Chunk) uint64 {
	max := chunks[0].EndSlot
	for _, c := range chunks[1:] {
		if c.EndSlot > max {
			max = c.EndSlot
		}
	}
	return max
}

func listenerHTTPURL(addr net.Addr) string {
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		return "http://" + addr.String()
	}

	host := tcpAddr.IP.String()
	if tcpAddr.IP == nil || tcpAddr.IP.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(tcpAddr.Port))
}

func validateHTTPURL(value, name string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("%s is not a valid URL: %w", name, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%s must use http or https", name)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%s must include a host", name)
	}
	return nil
}

func expandPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	return path, nil
}

func resolveExecutable(path string) (string, error) {
	if strings.Contains(path, string(os.PathSeparator)) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve engine binary path %q: %w", path, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return "", fmt.Errorf("stat engine binary %q: %w", abs, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("engine binary %q is a directory", abs)
		}
		return abs, nil
	}

	resolved, err := exec.LookPath(path)
	if err != nil {
		return "", fmt.Errorf("find engine binary %q on PATH: %w", path, err)
	}
	return resolved, nil
}

func simulatorVersion() string {
	if version != "" && version != "dev" {
		return version
	}

	cmd := exec.Command("git", "rev-parse", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return version
	}

	sha := strings.TrimSpace(string(output))
	if sha == "" {
		return version
	}
	return sha
}

func cleanupWorkerCache(workersDir string, stdout io.Writer) {
	if err := os.RemoveAll(workersDir); err != nil {
		fmt.Fprintf(stdout, "warning: failed to remove worker cache %s: %v\n", workersDir, err)
	}
}
