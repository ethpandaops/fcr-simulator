package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
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
	"github.com/ethpandaops/fcr-simulator/pkg/blockarchive"
	"github.com/ethpandaops/fcr-simulator/pkg/chunk"
	"github.com/ethpandaops/fcr-simulator/pkg/era"
	"github.com/ethpandaops/fcr-simulator/pkg/manifest"
	"github.com/ethpandaops/fcr-simulator/pkg/merge"
	"github.com/ethpandaops/fcr-simulator/pkg/s3cache"
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
	defaultFirstSeenDeadlineMS   = uint64(12000)
	defaultLookaheadCap          = uint64(4)
	defaultHTTPListen            = "127.0.0.1:0"
	defaultDecodedBlockCacheSize = beaconapi.DefaultDecodedBlockCacheSize
	defaultS3PathStyle           = true
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
	engineBinarySource    engineBinarySource
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
	FirstSeenBasePath     string
	FirstSeenDeadlineMS   uint64
	LookaheadCap          uint64
	HTTPListen            string
	BlockArchiveURL       string
	DecodedBlockCacheSize int
	S3Endpoint            string
	S3Bucket              string
	S3PathStyle           bool
	KeepCache             bool
	PrepOnly              bool
}

type eraMirrorConfig struct {
	Network      string
	StartEpoch   uint64
	EndEpoch     uint64
	WarmupEpochs uint64
	Parallel     int
	EraURL       string
	CacheDir     string
	S3Endpoint   string
	S3Bucket     string
	S3PathStyle  bool
}

type engineBinarySource int

const (
	engineBinarySourceDefault engineBinarySource = iota
	engineBinarySourceEnv
	engineBinarySourceFlag
)

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
	if len(args) > 0 && args[0] == "era-mirror" {
		return runEraMirrorCommand(ctx, args[1:], stdout, stderr)
	}

	cfg, printVersion, err := parseConfig(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	if printVersion {
		fmt.Fprintf(stdout, "fcr-orchestrator %s\n", simulatorVersion())
		return 0
	}

	if err := prepareEngineBinary(ctx, &cfg, stderr); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
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
	fs.StringVar(&cfg.EngineBinary, "engine-binary", "", "optional path to engine binary (env: FCR_ENGINE_BINARY; default: ./results/fcr-<engine>, auto-builds via engines/<engine>/build.sh if missing)")
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
	fs.StringVar(&cfg.AttestationSourceMode, "attestation-source-mode", defaultAttestationSourceMode, "next-non-missed, strict-source-block-k-minus-1, greedy-lookahead, or xatu-first-seen-singles")
	fs.StringVar(&cfg.FirstSeenBasePath, "attestation-first-seen-base", "", "base path for first-seen parquet files (local path or s3://bucket/prefix)")
	fs.Uint64Var(&cfg.FirstSeenDeadlineMS, "attestation-first-seen-deadline-ms", defaultFirstSeenDeadlineMS, "first-seen raw_seen_ms deadline for xatu-first-seen-singles mode")
	fs.Uint64Var(&cfg.LookaheadCap, "lookahead-cap", defaultLookaheadCap, "attestation lookahead cap")
	fs.StringVar(&cfg.HTTPListen, "http-listen", defaultHTTPListen, "local HTTP listen address")
	fs.StringVar(&cfg.BlockArchiveURL, "block-archive-url", "", "optional block-archiver URL for resolving attestations to orphan/non-canonical blocks")
	fs.IntVar(&cfg.DecodedBlockCacheSize, "decoded-block-cache-size", defaultDecodedBlockCacheSize, "decoded block LRU entries for per-slot simulation planning (0 disables)")
	fs.StringVar(&cfg.S3Endpoint, "s3-endpoint", os.Getenv("S3_ENDPOINT"), "optional S3-compatible cache endpoint (env: S3_ENDPOINT)")
	fs.StringVar(&cfg.S3Bucket, "s3-bucket", os.Getenv("S3_BUCKET"), "optional S3-compatible cache bucket (env: S3_BUCKET)")
	fs.BoolVar(&cfg.S3PathStyle, "s3-path-style", defaultS3PathStyle, "use S3 path-style addressing")
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

	engineBinaryFlagSet := flagWasSet(fs, "engine-binary")
	switch {
	case engineBinaryFlagSet:
		if cfg.EngineBinary == "" {
			return config{}, false, fmt.Errorf("--engine-binary cannot be empty")
		}
		cfg.engineBinarySource = engineBinarySourceFlag
	case os.Getenv("FCR_ENGINE_BINARY") != "":
		cfg.EngineBinary = os.Getenv("FCR_ENGINE_BINARY")
		cfg.engineBinarySource = engineBinarySourceEnv
	default:
		cfg.engineBinarySource = engineBinarySourceDefault
	}

	cfg.StartEpoch = startEpoch.value
	cfg.EndEpoch = endEpoch.value
	if err := validateConfig(&cfg, startEpoch.set, endEpoch.set); err != nil {
		return config{}, false, err
	}
	if cfg.engineBinarySource == engineBinarySourceDefault {
		cfg.EngineBinary = defaultEngineBinaryPath(cfg.Engine)
	}

	expandedCacheDir, err := expandPath(cfg.CacheDir)
	if err != nil {
		return config{}, false, err
	}
	cfg.CacheDir = expandedCacheDir

	cfg.BeaconNodeURL = strings.TrimRight(strings.TrimSpace(cfg.BeaconNodeURL), "/")
	cfg.EraURL = strings.TrimRight(strings.TrimSpace(cfg.EraURL), "/")
	cfg.BlockArchiveURL = strings.TrimRight(strings.TrimSpace(cfg.BlockArchiveURL), "/")
	cfg.S3Endpoint = strings.TrimRight(strings.TrimSpace(cfg.S3Endpoint), "/")
	cfg.S3Bucket = strings.TrimSpace(cfg.S3Bucket)
	cfg.OutputFormat = strings.ToLower(strings.TrimSpace(cfg.OutputFormat))
	cfg.AttestationSourceMode = strings.TrimSpace(cfg.AttestationSourceMode)
	cfg.FirstSeenBasePath = strings.TrimSpace(cfg.FirstSeenBasePath)
	if cfg.FirstSeenBasePath != "" && !isS3URI(cfg.FirstSeenBasePath) {
		expandedFirstSeenBase, err := expandPath(cfg.FirstSeenBasePath)
		if err != nil {
			return config{}, false, err
		}
		cfg.FirstSeenBasePath = expandedFirstSeenBase
	}
	if cfg.AttestationSourceMode == "strict-source-block-k-minus-1" || cfg.AttestationSourceMode == "xatu-first-seen-singles" {
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
	if strings.TrimSpace(cfg.BlockArchiveURL) != "" {
		if err := validateHTTPURL(cfg.BlockArchiveURL, "--block-archive-url"); err != nil {
			return err
		}
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
	case "next-non-missed", "greedy-lookahead":
		if cfg.LookaheadCap == 0 {
			return fmt.Errorf("--lookahead-cap must be greater than zero for %s mode", cfg.AttestationSourceMode)
		}
	case "strict-source-block-k-minus-1":
	case "xatu-first-seen-singles":
		if cfg.Engine != "lighthouse" {
			return fmt.Errorf("--attestation-source-mode xatu-first-seen-singles is currently supported only with --engine lighthouse")
		}
		if strings.TrimSpace(cfg.FirstSeenBasePath) == "" {
			return fmt.Errorf("--attestation-first-seen-base is required for xatu-first-seen-singles mode")
		}
		if isS3URI(cfg.FirstSeenBasePath) {
			if err := validateFirstSeenS3Settings(cfg.S3Endpoint); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("--attestation-source-mode must be next-non-missed, strict-source-block-k-minus-1, greedy-lookahead, or xatu-first-seen-singles")
	}
	if strings.TrimSpace(cfg.HTTPListen) == "" {
		return fmt.Errorf("--http-listen is required")
	}
	if cfg.DecodedBlockCacheSize < 0 {
		return fmt.Errorf("--decoded-block-cache-size must be >= 0")
	}
	if err := validateOptionalCacheS3Settings(cfg.S3Endpoint, cfg.S3Bucket, isS3URI(cfg.FirstSeenBasePath)); err != nil {
		return err
	}
	return nil
}

func runEraMirrorCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, err := parseEraMirrorConfig(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, err)
		return 1
	}

	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "create cache directory %q: %v\n", cfg.CacheDir, err)
		return 1
	}

	s3Store, err := newS3Store(cfg.S3Endpoint, cfg.S3Bucket, cfg.S3PathStyle)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if s3Store == nil {
		fmt.Fprintln(stderr, "S3 cache is required for era-mirror")
		return 1
	}

	chunks := chunk.Split(cfg.StartEpoch, cfg.EndEpoch, cfg.WarmupEpochs, cfg.Parallel)
	activeChunks := filterActiveChunks(chunks)
	if len(activeChunks) == 0 {
		fmt.Fprintln(stderr, "no non-empty chunks generated")
		return 1
	}

	startSlot := minWarmupSlot(activeChunks)
	endSlot := maxEndSlot(activeChunks)
	startEra, endEra := eraRangeForSlots(startSlot, endSlot)

	downloader, err := era.NewDownloaderWithS3(cfg.EraURL, cfg.CacheDir, cfg.Network, s3Store)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	fmt.Fprintf(stdout, "mirroring ERA files for slots %d through %d into s3://%s/era/%s/ (eras %d through %d)\n",
		startSlot, endSlot, cfg.S3Bucket, cfg.Network, startEra, endEra)
	stats, err := downloader.MirrorToS3(ctx, startEra, endEra, cfg.Parallel)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "ERA mirror complete: scanned=%d skipped=%d downloaded=%d uploaded=%d\n",
		stats.Scanned, stats.Skipped, stats.Downloaded, stats.Uploaded)
	return 0
}

func parseEraMirrorConfig(args []string, output io.Writer) (eraMirrorConfig, error) {
	var cfg eraMirrorConfig
	var startEpoch requiredUint64Flag
	var endEpoch requiredUint64Flag

	fs := flag.NewFlagSet("fcr-orchestrator era-mirror", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&cfg.Network, "network", "mainnet", "network name (V1 supports mainnet)")
	fs.Var(&startEpoch, "start-epoch", "first epoch, inclusive")
	fs.Var(&endEpoch, "end-epoch", "end epoch, exclusive")
	fs.Uint64Var(&cfg.WarmupEpochs, "warmup-epochs", defaultWarmupEpochs, "warmup epochs per worker")
	fs.IntVar(&cfg.Parallel, "parallel", defaultParallel, "number of workers")
	fs.StringVar(&cfg.EraURL, "era-url", defaultEraURL, "ERA file base URL")
	fs.StringVar(&cfg.CacheDir, "cache-dir", defaultCacheDir, "cache directory")
	fs.StringVar(&cfg.S3Endpoint, "s3-endpoint", os.Getenv("S3_ENDPOINT"), "S3-compatible cache endpoint (env: S3_ENDPOINT)")
	fs.StringVar(&cfg.S3Bucket, "s3-bucket", os.Getenv("S3_BUCKET"), "S3-compatible cache bucket (env: S3_BUCKET)")
	fs.BoolVar(&cfg.S3PathStyle, "s3-path-style", defaultS3PathStyle, "use S3 path-style addressing")

	if err := fs.Parse(args); err != nil {
		return eraMirrorConfig{}, err
	}
	if fs.NArg() != 0 {
		return eraMirrorConfig{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}

	cfg.StartEpoch = startEpoch.value
	cfg.EndEpoch = endEpoch.value
	if err := validateEraMirrorConfig(&cfg, startEpoch.set, endEpoch.set); err != nil {
		return eraMirrorConfig{}, err
	}

	expandedCacheDir, err := expandPath(cfg.CacheDir)
	if err != nil {
		return eraMirrorConfig{}, err
	}
	cfg.CacheDir = expandedCacheDir
	cfg.Network = strings.TrimSpace(cfg.Network)
	cfg.EraURL = strings.TrimRight(strings.TrimSpace(cfg.EraURL), "/")
	cfg.S3Endpoint = strings.TrimRight(strings.TrimSpace(cfg.S3Endpoint), "/")
	cfg.S3Bucket = strings.TrimSpace(cfg.S3Bucket)
	return cfg, nil
}

func validateEraMirrorConfig(cfg *eraMirrorConfig, startSet, endSet bool) error {
	if strings.TrimSpace(cfg.Network) == "" {
		return fmt.Errorf("--network is required")
	}
	if strings.TrimSpace(cfg.Network) != "mainnet" {
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
	if strings.TrimSpace(cfg.EraURL) == "" {
		return fmt.Errorf("--era-url is required")
	}
	if err := validateHTTPURL(cfg.EraURL, "--era-url"); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.CacheDir) == "" {
		return fmt.Errorf("--cache-dir is required")
	}
	return validateS3Settings(cfg.S3Endpoint, cfg.S3Bucket, true)
}

func execute(ctx context.Context, cfg config, stdout io.Writer) (int, error) {
	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		return 1, fmt.Errorf("create cache directory %q: %w", cfg.CacheDir, err)
	}

	s3Store, err := newS3Store(cfg.S3Endpoint, cfg.S3Bucket, cfg.S3PathStyle)
	if err != nil {
		return 1, err
	}
	if s3Store != nil {
		fmt.Fprintf(stdout, "S3 cache enabled: bucket %s at %s\n", cfg.S3Bucket, cfg.S3Endpoint)
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

	initialEraStartSlot := minWarmupSlot(activeChunks)
	initialEraEndSlot := maxEndSlot(activeChunks)
	fmt.Fprintf(stdout, "pre-downloading ERA files for slots %d through %d\n", initialEraStartSlot, initialEraEndSlot)
	downloader, err := era.NewDownloaderWithS3(cfg.EraURL, cfg.CacheDir, cfg.Network, s3Store)
	if err != nil {
		return 1, err
	}
	if err := downloader.PreDownloadContext(ctx, initialEraStartSlot, initialEraEndSlot); err != nil {
		return 1, fmt.Errorf("pre-download ERA files: %w", err)
	}

	eraReader, err := era.New(downloader.CacheDir())
	if err != nil {
		return 1, err
	}

	fetcher, err := beaconfetch.NewWithS3(cfg.BeaconNodeURL, cfg.CacheDir, cfg.Network, s3Store)
	if err != nil {
		return 1, err
	}

	firstSeenSource, err := newFirstSeenSource(cfg)
	if err != nil {
		return 1, err
	}

	workerInfos, checkpointStates, checkpointBlocks, err := prepareWorkers(cfg, fetcher, chunks, stdout)
	if err != nil {
		return 1, err
	}
	initialEraEnvelope := eraSlotEnvelope{
		StartSlot: initialEraStartSlot,
		EndSlot:   saturatingSlotAdd(initialEraEndSlot, era.PreDownloadLookaheadSlots),
	}
	if err := preDownloadWarmArchiveCatchUpEras(ctx, cfg, downloader, workerInfos, initialEraEnvelope, initialEraEndSlot, stdout); err != nil {
		return 1, fmt.Errorf("catch-up pre-download ERA files: %w", err)
	}

	fmt.Fprintln(stdout, "fetching genesis state")
	if _, err := fetcher.FetchGenesisStateSSZ(); err != nil {
		return 1, fmt.Errorf("fetch genesis state: %w", err)
	}

	archiveWarm, err := warmBlockArchiveCache(cfg, eraReader, workerInfos, checkpointBlocks, firstSeenSource, stdout)
	if err != nil {
		return 1, fmt.Errorf("warm block archive cache: %w", err)
	}

	if cfg.PrepOnly {
		fmt.Fprintf(stdout, "prep complete: %d checkpoint state(s) cached for %d worker(s)\n",
			len(checkpointStates), len(workerInfos))
		return 0, nil
	}

	server, serverURL, shutdown, err := startBeaconAPIServer(cfg, eraReader, fetcher, checkpointBlocks, archiveWarm.OrphanRootsBySlot, firstSeenSource)
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
	canonicalLookup := beaconapi.NewCanonicalRootLookup(beaconapi.RealBackendConfig{
		EraReader:             eraReader,
		ForkSchedule:          beaconapi.ForkSchedule{SlotFork: beaconapi.MainnetForkAtSlot},
		DecodedBlockCacheSize: cfg.DecodedBlockCacheSize,
	}, minWarmupSlot(activeChunks), saturatingSlotAdd(maxEndSlot(activeChunks), cfg.LookaheadCap))
	stats, err := merge.MergeAndWriteWithCanonical(mergePaths, paths.JSONL, paths.CSV, schema.OrchestratorMetadata{
		EngineName:            engineManifest.EngineName,
		EngineVersion:         engineManifest.EngineVersion,
		EngineCommit:          engineManifest.EngineCommit,
		AttestationSourceMode: cfg.AttestationSourceMode,
		LookaheadCap:          cfg.LookaheadCap,
	}, expectedSlots, canonicalLookup.Lookup)
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
		if actualSlot != c.WarmupStartSlot {
			fmt.Fprintf(stdout, "worker %d warmup slot adjusted: planned=%d actual=%d\n", c.Index, c.WarmupStartSlot, actualSlot)
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

type eraSlotEnvelope struct {
	StartSlot uint64
	EndSlot   uint64
}

func preDownloadWarmArchiveCatchUpEras(ctx context.Context, cfg config, downloader *era.Downloader, workerInfos []workerInfo, initialCovered eraSlotEnvelope, maxPlannedEndSlot uint64, stdout io.Writer) error {
	envelope, ok := warmArchiveEraDownloadEnvelope(workerInfos, maxPlannedEndSlot, cfg.LookaheadCap)
	if !ok || !slotEnvelopeExtendsBeyond(envelope, initialCovered) {
		return nil
	}

	startEra, endEra := eraRangeForExactSlotRange(envelope.StartSlot, envelope.EndSlot)
	fmt.Fprintf(stdout, "catching up ERA files for warm archive slots %d through %d (eras %d through %d)\n", envelope.StartSlot, envelope.EndSlot, startEra, endEra)
	return downloader.PreDownloadSlotRangeContext(ctx, envelope.StartSlot, envelope.EndSlot)
}

func warmArchiveEraDownloadEnvelope(workerInfos []workerInfo, maxPlannedEndSlot, lookaheadCap uint64) (eraSlotEnvelope, bool) {
	minActualWarmupSlot, ok := minActualWorkerWarmupSlot(workerInfos)
	if !ok {
		return eraSlotEnvelope{}, false
	}
	return eraSlotEnvelope{
		StartSlot: oneEraGuardedStartSlot(minActualWarmupSlot),
		EndSlot:   saturatingSlotAdd(maxPlannedEndSlot, lookaheadCap),
	}, true
}

func minActualWorkerWarmupSlot(workerInfos []workerInfo) (uint64, bool) {
	var min uint64
	found := false
	for _, info := range workerInfos {
		if info.Skipped {
			continue
		}
		if !found || info.ActualWarmupStartSlot < min {
			min = info.ActualWarmupStartSlot
			found = true
		}
	}
	return min, found
}

func oneEraGuardedStartSlot(slot uint64) uint64 {
	if slot <= era.SlotsPerEra {
		return 0
	}
	return slot - era.SlotsPerEra
}

func slotEnvelopeExtendsBeyond(candidate, covered eraSlotEnvelope) bool {
	return candidate.StartSlot < covered.StartSlot || candidate.EndSlot > covered.EndSlot
}

func newArchiveClient(cfg config) (*blockarchive.Client, error) {
	if cfg.BlockArchiveURL == "" {
		return nil, nil
	}
	return blockarchive.New(
		cfg.BlockArchiveURL,
		cfg.Network,
		filepath.Join(cfg.CacheDir, "block-archive"),
	)
}

func newFirstSeenSource(cfg config) (*beaconapi.FirstSeenAttestationSource, error) {
	if cfg.AttestationSourceMode != "xatu-first-seen-singles" {
		return nil, nil
	}

	s3Store, err := newFirstSeenS3Store(cfg)
	if err != nil {
		return nil, err
	}
	return beaconapi.NewFirstSeenAttestationSource(beaconapi.FirstSeenAttestationSourceConfig{
		BasePath:   cfg.FirstSeenBasePath,
		Network:    cfg.Network,
		DeadlineMS: cfg.FirstSeenDeadlineMS,
		CacheDir:   cfg.CacheDir,
		S3Store:    s3Store,
	})
}

// warmBlockArchiveCache front-loads every orphan block the engine workers will
// request into the local disk cache, so the serving hot loop never contacts the
// block archive over HTTP (where a slow or timing-out origin would stall or fail
// a running simulation). No-op when no archive is configured.
func warmBlockArchiveCache(cfg config, eraReader *era.Reader, workerInfos []workerInfo, checkpointBlocks map[[32]byte][]byte, firstSeenSource *beaconapi.FirstSeenAttestationSource, stdout io.Writer) (beaconapi.ArchiveWarmResult, error) {
	archiveClient, err := newArchiveClient(cfg)
	if err != nil {
		return beaconapi.ArchiveWarmResult{}, err
	}
	if archiveClient == nil {
		return beaconapi.ArchiveWarmResult{}, nil
	}

	mode, err := parseAttplanMode(cfg.AttestationSourceMode)
	if err != nil {
		return beaconapi.ArchiveWarmResult{}, err
	}

	ranges := make([][2]uint64, 0, len(workerInfos))
	for _, info := range workerInfos {
		if info.Skipped {
			continue
		}
		from := info.ActualWarmupStartSlot + 1
		to := info.Chunk.EndSlot
		if from >= to {
			continue
		}
		ranges = append(ranges, [2]uint64{from, to})
	}
	if len(ranges) == 0 {
		result := beaconapi.ArchiveWarmResult{}
		logArchiveWarmSummary(stdout, result.Stats)
		return result, nil
	}

	fmt.Fprintf(stdout, "warming block-archive cache for %d worker range(s)\n", len(ranges))
	result, err := beaconapi.WarmBlockArchiveCache(beaconapi.RealBackendConfig{
		EraReader:              eraReader,
		GenesisInfo:            mainnetGenesisInfo(),
		ForkSchedule:           beaconapi.ForkSchedule{SlotFork: beaconapi.MainnetForkAtSlot},
		Mode:                   mode,
		LookaheadCap:           cfg.LookaheadCap,
		CheckpointBlocksByRoot: checkpointBlocks,
		BlockArchive:           archiveClient,
		DecodedBlockCacheSize:  cfg.DecodedBlockCacheSize,
		FirstSeenSource:        firstSeenSource,
	}, ranges)
	logArchiveWarmSummary(stdout, result.Stats)
	return result, err
}

func logArchiveWarmSummary(stdout io.Writer, stats beaconapi.ArchiveWarmStats) {
	fmt.Fprintf(
		stdout,
		"block-archive warm summary: worker_ranges=%d slot_range_queries=%d orphan_roots_discovered=%d high_orphan_fanout_slots=%d roots_resolved_cached=%d roots_archive_missing=%d transient_failures_retried=%d chains_truncated=%d chains_pre_warmup_parent=%d\n",
		stats.WorkerRanges,
		stats.SlotRangeQueries,
		stats.OrphanRootsDiscovered,
		stats.HighOrphanFanoutSlots,
		stats.RootsResolvedCached,
		stats.RootsArchiveMissing,
		stats.TransientFailuresRetried,
		stats.ChainsTruncated,
		stats.ChainsPreWarmupParent,
	)
}

func startBeaconAPIServer(cfg config, eraReader *era.Reader, fetcher *beaconfetch.Fetcher, checkpointBlocks map[[32]byte][]byte, archiveOrphanRootsBySlot map[uint64][][32]byte, firstSeenSource *beaconapi.FirstSeenAttestationSource) (*http.Server, string, func(), error) {
	mode, err := parseAttplanMode(cfg.AttestationSourceMode)
	if err != nil {
		return nil, "", nil, err
	}

	archiveClient, err := newArchiveClient(cfg)
	if err != nil {
		return nil, "", nil, err
	}

	backend := beaconapi.NewRealBackend(beaconapi.RealBackendConfig{
		EraReader:                eraReader,
		Fetcher:                  fetcher,
		GenesisInfo:              mainnetGenesisInfo(),
		ForkSchedule:             beaconapi.ForkSchedule{SlotFork: beaconapi.MainnetForkAtSlot},
		Mode:                     mode,
		LookaheadCap:             cfg.LookaheadCap,
		CheckpointBlocksByRoot:   checkpointBlocks,
		BlockArchive:             archiveClient,
		ArchiveCacheOnly:         true,
		ArchiveOrphanRootsBySlot: archiveOrphanRootsBySlot,
		DecodedBlockCacheSize:    cfg.DecodedBlockCacheSize,
		FirstSeenSource:          firstSeenSource,
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

	firstSeenBasePath := ""
	firstSeenDeadlineMS := uint64(0)
	if cfg.AttestationSourceMode == "xatu-first-seen-singles" {
		firstSeenBasePath = cfg.FirstSeenBasePath
		firstSeenDeadlineMS = cfg.FirstSeenDeadlineMS
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
			FirstSeenBasePath:     firstSeenBasePath,
			FirstSeenDeadlineMS:   firstSeenDeadlineMS,
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
	case "greedy-lookahead":
		return attplan.ModeGreedyLookahead, nil
	case "xatu-first-seen-singles":
		return attplan.ModeFirstSeenGossip, nil
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

func saturatingSlotAdd(value, delta uint64) uint64 {
	if delta > math.MaxUint64-value {
		return math.MaxUint64
	}
	return value + delta
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

func newS3Store(endpoint, bucket string, pathStyle bool) (s3cache.Store, error) {
	if strings.TrimSpace(endpoint) == "" && strings.TrimSpace(bucket) == "" {
		return nil, nil
	}
	if strings.TrimSpace(bucket) == "" {
		return nil, nil
	}
	return s3cache.New(s3cache.Config{
		Endpoint:        endpoint,
		Bucket:          bucket,
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		PathStyle:       pathStyle,
	})
}

func newFirstSeenS3Store(cfg config) (s3cache.Store, error) {
	if !isS3URI(cfg.FirstSeenBasePath) {
		return nil, nil
	}
	bucket, err := s3BucketFromURI(cfg.FirstSeenBasePath)
	if err != nil {
		return nil, err
	}
	return s3cache.New(s3cache.Config{
		Endpoint:        cfg.S3Endpoint,
		Bucket:          bucket,
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		PathStyle:       cfg.S3PathStyle,
	})
}

func validateOptionalCacheS3Settings(endpoint, bucket string, endpointMayBeFirstSeenOnly bool) error {
	endpoint = strings.TrimSpace(endpoint)
	bucket = strings.TrimSpace(bucket)
	if endpointMayBeFirstSeenOnly && endpoint != "" && bucket == "" {
		return nil
	}
	return validateS3Settings(endpoint, bucket, false)
}

func validateFirstSeenS3Settings(endpoint string) error {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return fmt.Errorf("--s3-endpoint is required when --attestation-first-seen-base uses s3://")
	}
	if strings.TrimSpace(os.Getenv("AWS_ACCESS_KEY_ID")) == "" {
		return fmt.Errorf("AWS_ACCESS_KEY_ID is required when --attestation-first-seen-base uses s3://")
	}
	if strings.TrimSpace(os.Getenv("AWS_SECRET_ACCESS_KEY")) == "" {
		return fmt.Errorf("AWS_SECRET_ACCESS_KEY is required when --attestation-first-seen-base uses s3://")
	}
	return validateS3Endpoint(endpoint)
}

func isS3URI(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), "s3://")
}

func s3BucketFromURI(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", fmt.Errorf("--attestation-first-seen-base is not a valid S3 URI: %w", err)
	}
	if parsed.Scheme != "s3" {
		return "", fmt.Errorf("--attestation-first-seen-base must use s3://")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("--attestation-first-seen-base must include an S3 bucket")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("--attestation-first-seen-base must not include query or fragment")
	}
	return parsed.Host, nil
}

func validateS3Settings(endpoint, bucket string, required bool) error {
	endpoint = strings.TrimSpace(endpoint)
	bucket = strings.TrimSpace(bucket)
	if !required && endpoint == "" && bucket == "" {
		return nil
	}
	if endpoint == "" {
		return fmt.Errorf("--s3-endpoint is required when S3 cache is configured")
	}
	if bucket == "" {
		return fmt.Errorf("--s3-bucket is required when S3 cache is configured")
	}
	if strings.TrimSpace(os.Getenv("AWS_ACCESS_KEY_ID")) == "" {
		return fmt.Errorf("AWS_ACCESS_KEY_ID is required when S3 cache is configured")
	}
	if strings.TrimSpace(os.Getenv("AWS_SECRET_ACCESS_KEY")) == "" {
		return fmt.Errorf("AWS_SECRET_ACCESS_KEY is required when S3 cache is configured")
	}
	if err := validateS3Endpoint(endpoint); err != nil {
		return err
	}
	return nil
}

func validateS3Endpoint(value string) error {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("--s3-endpoint is not a valid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("--s3-endpoint must use http or https")
	}
	if parsed.Host == "" {
		return fmt.Errorf("--s3-endpoint must include a host")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return fmt.Errorf("--s3-endpoint must not include a path")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("--s3-endpoint must not include query or fragment")
	}
	return nil
}

func eraRangeForSlots(startSlot, endSlot uint64) (uint64, uint64) {
	endWithLookahead := saturatingSlotAdd(endSlot, era.PreDownloadLookaheadSlots)
	return era.EraNumberForSlot(startSlot), era.EraNumberForSlot(endWithLookahead)
}

func eraRangeForExactSlotRange(startSlot, endSlot uint64) (uint64, uint64) {
	return era.EraNumberForSlot(startSlot), era.EraNumberForSlot(endSlot)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func defaultEngineBinaryPath(engine string) string {
	return "." + string(os.PathSeparator) + filepath.Join("results", "fcr-"+engine)
}

func defaultEngineBuildScript(engine string) string {
	return filepath.Join("engines", engine, "build.sh")
}

func prepareEngineBinary(ctx context.Context, cfg *config, stderr io.Writer) error {
	if cfg.engineBinarySource != engineBinarySourceDefault {
		engineBinary, err := resolveExecutable(cfg.EngineBinary)
		if err != nil {
			return err
		}
		cfg.EngineBinary = engineBinary
		return nil
	}

	engineBinary, err := resolveExecutable(cfg.EngineBinary)
	if err == nil {
		cfg.EngineBinary = engineBinary
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	buildScript := defaultEngineBuildScript(cfg.Engine)
	info, statErr := os.Stat(buildScript)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("engine binary not found at %s; build it via %s or pass --engine-binary <path>", cfg.EngineBinary, buildScript)
		}
		return fmt.Errorf("stat engine build script %q: %w", buildScript, statErr)
	}
	if info.IsDir() {
		return fmt.Errorf("engine build script %q is a directory", buildScript)
	}

	fmt.Fprintf(stderr, "binary not found, running %s...\n", buildScript)
	cmd := exec.CommandContext(ctx, buildScript)
	cmd.Stdout = stderr
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run %s: %w", buildScript, err)
	}

	engineBinary, err = resolveExecutable(cfg.EngineBinary)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("engine build script %s completed but engine binary not found at %s", buildScript, cfg.EngineBinary)
		}
		return err
	}
	cfg.EngineBinary = engineBinary
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
