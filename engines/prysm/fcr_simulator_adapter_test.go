package forkchoice

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OffchainLabs/go-bitfield"
	"github.com/OffchainLabs/prysm/v7/beacon-chain/core/helpers"
	"github.com/OffchainLabs/prysm/v7/beacon-chain/das"
	beaconstate "github.com/OffchainLabs/prysm/v7/beacon-chain/state"
	"github.com/OffchainLabs/prysm/v7/config/params"
	"github.com/OffchainLabs/prysm/v7/consensus-types/primitives"
	"github.com/OffchainLabs/prysm/v7/encoding/ssz/detect"
	ethpb "github.com/OffchainLabs/prysm/v7/proto/prysm/v1alpha1"
	attestationutil "github.com/OffchainLabs/prysm/v7/proto/prysm/v1alpha1/attestation"
)

const engineCommit = "5fc7c60dc7ba7008ce8fab1b7ca0336a44a78fec"

var cli = struct {
	manifest           bool
	beaconNodeURL      string
	startSlot          uint64
	endSlot            uint64
	warmupStartSlot    uint64
	network            string
	byzantineThreshold uint64
	attestationMode    string
	lookaheadCap       uint64
	output             string
}{}

func init() {
	flag.BoolVar(&cli.manifest, "manifest-json", false, "print engine manifest")
	flag.StringVar(&cli.beaconNodeURL, "beacon-node-url", "", "simulator beacon API URL")
	flag.Uint64Var(&cli.startSlot, "start-slot", 0, "first output slot")
	flag.Uint64Var(&cli.endSlot, "end-slot", 0, "exclusive output slot")
	flag.Uint64Var(&cli.warmupStartSlot, "warmup-start-slot", 0, "checkpoint slot")
	flag.StringVar(&cli.network, "network", "", "network name")
	flag.Uint64Var(&cli.byzantineThreshold, "byzantine-threshold", 25, "FCR Byzantine threshold")
	flag.StringVar(&cli.attestationMode, "attestation-source-mode", "", "attestation source mode")
	flag.Uint64Var(&cli.lookaheadCap, "lookahead-cap", 0, "attestation lookahead cap")
	flag.StringVar(&cli.output, "output", "", "output JSONL path")
}

func TestMain(m *testing.M) {
	flag.Parse()
	if cli.manifest {
		manifest := map[string]any{
			"engine_name":     "prysm",
			"engine_version":  "unreleased-pr-17122",
			"engine_commit":   engineCommit,
			"build_flags":     []string{"fake_crypto", "fcr_pr_17122", "test_harness", "no_signature_verification"},
			"fcr_spec_commit": "",
		}
		_ = json.NewEncoder(os.Stdout).Encode(manifest)
		os.Exit(0)
	}
	_ = flag.Set("test.run", "^TestFCRSimulator$")
	os.Exit(m.Run())
}

type slotResponse struct {
	Version uint64          `json:"version"`
	Slot    slotInstruction `json:"slot"`
}

type slotInstruction struct {
	SimSlot      uint64      `json:"sim_slot"`
	EvalSlot     uint64      `json:"eval_slot"`
	ImportBlocks []planBlock `json:"import_blocks"`
	Attestations []planAtt   `json:"attestations"`
}

type planBlock struct {
	Slot      uint64 `json:"slot"`
	Root      string `json:"root"`
	Canonical bool   `json:"canonical"`
}

type planAtt struct {
	AggregationBits  string   `json:"aggregation_bits"`
	CommitteeBits    string   `json:"committee_bits"`
	AttestingIndices []uint64 `json:"attesting_indices"`
	Data             struct {
		Slot            uint64 `json:"slot"`
		Index           uint64 `json:"index"`
		BeaconBlockRoot string `json:"beacon_block_root"`
		Source          struct {
			Epoch uint64 `json:"epoch"`
			Root  string `json:"root"`
		} `json:"source"`
		Target struct {
			Epoch uint64 `json:"epoch"`
			Root  string `json:"root"`
		} `json:"target"`
	} `json:"data"`
}

type outputRecord struct {
	Slot                    uint64  `json:"slot"`
	Epoch                   uint64  `json:"epoch"`
	HasBlock                bool    `json:"has_block"`
	BlockRoot               *string `json:"block_root"`
	HeadRoot                string  `json:"head_root"`
	ConfirmedRoot           string  `json:"confirmed_root"`
	ConfirmedSlot           uint64  `json:"confirmed_slot"`
	ConfirmationDelaySlots  uint64  `json:"confirmation_delay_slots"`
	FastConfirmed           bool    `json:"fast_confirmed"`
	StrictOneSlotConfirmed  bool    `json:"strict_one_slot_confirmed"`
	FinalizedEpoch          uint64  `json:"finalized_epoch"`
	JustifiedEpoch          uint64  `json:"justified_epoch"`
	SourceBlockSlot         *uint64 `json:"source_block_slot"`
	NumAttestationsInjected uint64  `json:"num_attestations_injected"`
	IsEpochBoundary         bool    `json:"is_epoch_boundary"`
	IsMissedSlot            bool    `json:"is_missed_slot"`
	FCREvalDurationUS       uint64  `json:"fcr_eval_duration_us"`
}

func TestFCRSimulator(t *testing.T) {
	validateCLI(t)

	cfg := params.MainnetConfig()
	cfg.ConfirmationByzantineThreshold = cli.byzantineThreshold
	if err := params.SetActive(cfg); err != nil {
		t.Fatalf("configure mainnet: %v", err)
	}

	stateBytes := getSSZ(t, fmt.Sprintf("%s/eth/v2/debug/beacon/states/%d", baseURL(), cli.warmupStartSlot))
	vu, err := detect.FromState(stateBytes)
	if err != nil {
		t.Fatalf("detect checkpoint state fork: %v", err)
	}
	anchorState, err := vu.UnmarshalBeaconState(stateBytes)
	if err != nil {
		t.Fatalf("decode checkpoint state: %v", err)
	}
	anchorBlockBytes := getSSZ(t, fmt.Sprintf("%s/eth/v2/beacon/blocks/%d", baseURL(), cli.warmupStartSlot))
	blockVU, err := detect.FromBlock(anchorBlockBytes)
	if err != nil {
		t.Fatalf("detect checkpoint block fork: %v", err)
	}
	anchorBlock, err := blockVU.UnmarshalBeaconBlock(anchorBlockBytes)
	if err != nil {
		t.Fatalf("decode checkpoint block: %v", err)
	}
	anchorRoot, err := anchorBlock.Block().HashTreeRoot()
	if err != nil {
		t.Fatalf("hash checkpoint block: %v", err)
	}

	builder := NewFCRBuilder(t, anchorState, anchorBlock)
	builder.service.InitFCRSimulatorPools()
	secondsPerSlot := params.BeaconConfig().SecondsPerSlot
	builder.lastTick = int64(cli.warmupStartSlot * secondsPerSlot)
	setBuilderTime(builder, cli.warmupStartSlot)

	out, err := os.Create(cli.output)
	if err != nil {
		t.Fatalf("create output: %v", err)
	}
	defer func() {
		if err := out.Close(); err != nil {
			t.Errorf("close output: %v", err)
		}
	}()
	encoder := json.NewEncoder(out)

	slotsByRoot := map[[32]byte]uint64{anchorRoot: uint64(anchorBlock.Block().Slot())}
	for slot := cli.warmupStartSlot + 1; slot < cli.endSlot; slot++ {
		builder.Tick(t, int64(slot*secondsPerSlot))
		// The simulator disables proposer boost. Import near the end of the slot
		// so Prysm does not enter its proposer-boost dependent-root path, whose
		// pre-anchor ancestry is intentionally absent from checkpoint replay.
		builder.service.SetForkChoiceGenesisTime(time.Now().Add(-time.Duration(slot*secondsPerSlot+secondsPerSlot-1) * time.Second))
		instruction := getInstruction(t, slot)

		var canonicalRoot *string
		for _, planned := range instruction.ImportBlocks {
			root := parseRoot(t, planned.Root)
			if planned.Canonical && planned.Slot == slot {
				value := strings.ToLower(planned.Root)
				canonicalRoot = &value
			}
			if builder.fc.HasNode(root) {
				continue
			}
			blockID := planned.Root
			if planned.Canonical {
				blockID = fmt.Sprint(planned.Slot)
			}
			blockBytes := getSSZ(t, fmt.Sprintf("%s/eth/v2/beacon/blocks/%s", baseURL(), blockID))
			blockVU, err := detect.FromBlock(blockBytes)
			if err != nil {
				t.Fatalf("slot %d detect block fork: %v", slot, err)
			}
			block, err := blockVU.UnmarshalBeaconBlock(blockBytes)
			if err != nil {
				t.Fatalf("slot %d decode block: %v", slot, err)
			}
			gotRoot, err := block.Block().HashTreeRoot()
			if err != nil || gotRoot != root {
				t.Fatalf("slot %d block root mismatch: got %#x, planned %s (err=%v)", slot, gotRoot, planned.Root, err)
			}
			if err := builder.service.ReceiveBlock(t.Context(), block, root, &das.MockAvailabilityStore{}); err != nil {
				t.Fatalf("slot %d import block: %v", slot, err)
			}
			slotsByRoot[root] = planned.Slot
		}

		// Keep the expensive head-state copy lazy for attesting_indices plans,
		// which do not need committee reconstruction.
		var headStateForCommittees beaconstate.ReadOnlyBeaconState
		for _, att := range instruction.Attestations {
			if len(att.AttestingIndices) > 0 {
				builder.fc.ProcessAttestation(t.Context(), att.AttestingIndices, parseRoot(t, att.Data.BeaconBlockRoot), primitives.Slot(att.Data.Slot), true)
				continue
			}
			blockRoot := parseRoot(t, att.Data.BeaconBlockRoot)
			sourceRoot := parseRoot(t, att.Data.Source.Root)
			targetRoot := parseRoot(t, att.Data.Target.Root)
			plannedAtt := &ethpb.AttestationElectra{
				AggregationBits: bitfield.Bitlist(parseHex(t, att.AggregationBits)),
				CommitteeBits:   bitfield.Bitvector64(parseHex(t, att.CommitteeBits)),
				Signature:       make([]byte, 96),
				Data: &ethpb.AttestationData{
					Slot: primitives.Slot(att.Data.Slot), CommitteeIndex: primitives.CommitteeIndex(att.Data.Index),
					BeaconBlockRoot: blockRoot[:],
					Source:          &ethpb.Checkpoint{Epoch: primitives.Epoch(att.Data.Source.Epoch), Root: sourceRoot[:]},
					Target:          &ethpb.Checkpoint{Epoch: primitives.Epoch(att.Data.Target.Epoch), Root: targetRoot[:]},
				},
			}
			if headStateForCommittees == nil {
				headState, err := builder.service.HeadState(t.Context())
				if err != nil {
					t.Fatalf("slot %d get head state for attestation: %v", slot, err)
				}
				headStateForCommittees = headState
			}
			committees, err := helpers.AttestationCommitteesFromState(t.Context(), headStateForCommittees, plannedAtt)
			if err != nil {
				t.Fatalf("slot %d compute attestation committees: %v", slot, err)
			}
			indices, err := attestationutil.AttestingIndices(plannedAtt, committees...)
			if err != nil {
				t.Fatalf("slot %d compute attesting indices: %v", slot, err)
			}
			builder.fc.ProcessAttestation(t.Context(), indices, blockRoot, primitives.Slot(att.Data.Slot), true)
		}

		start := time.Now()
		builder.Tick(t, int64(instruction.EvalSlot*secondsPerSlot))
		fcrEvalUS := uint64(time.Since(start).Microseconds())
		if slot < cli.startSlot {
			continue
		}

		head, err := builder.service.HeadRoot(t.Context())
		if err != nil {
			t.Fatalf("slot %d get head: %v", slot, err)
		}
		confirmed := builder.service.FCR().ConfirmedRoot()
		confirmedSlot := slotsByRoot[confirmed]
		delay := uint64(0)
		if instruction.EvalSlot >= confirmedSlot {
			delay = instruction.EvalSlot - confirmedSlot
		}
		confirmedHex := fmt.Sprintf("%#x", confirmed)
		hasBlock := canonicalRoot != nil
		strict := hasBlock && *canonicalRoot == confirmedHex && confirmedSlot == slot && delay == 1
		record := outputRecord{
			Slot: slot, Epoch: slot / uint64(params.BeaconConfig().SlotsPerEpoch),
			HasBlock: hasBlock, BlockRoot: canonicalRoot, HeadRoot: fmt.Sprintf("%#x", head),
			ConfirmedRoot: confirmedHex, ConfirmedSlot: confirmedSlot, ConfirmationDelaySlots: delay,
			FastConfirmed:           confirmed != [32]byte{} && confirmedSlot == slot,
			StrictOneSlotConfirmed:  strict,
			FinalizedEpoch:          uint64(builder.service.FinalizedCheckpt().Epoch),
			JustifiedEpoch:          uint64(builder.service.CurrentJustifiedCheckpt().Epoch),
			NumAttestationsInjected: uint64(len(instruction.Attestations)),
			IsEpochBoundary:         slot%uint64(params.BeaconConfig().SlotsPerEpoch) == 0,
			IsMissedSlot:            !hasBlock, FCREvalDurationUS: fcrEvalUS,
		}
		if err := encoder.Encode(record); err != nil {
			t.Fatalf("write slot %d: %v", slot, err)
		}
	}
	if err := out.Sync(); err != nil {
		t.Fatalf("sync output: %v", err)
	}
}

func validateCLI(t *testing.T) {
	t.Helper()
	if cli.network != "mainnet" {
		t.Fatalf("unsupported network %q; only mainnet is supported", cli.network)
	}
	if cli.beaconNodeURL == "" || cli.output == "" || cli.startSlot >= cli.endSlot || cli.warmupStartSlot > cli.startSlot {
		t.Fatal("invalid or incomplete simulator arguments")
	}
}

func baseURL() string { return strings.TrimRight(cli.beaconNodeURL, "/") }

func setBuilderTime(builder *Builder, slot uint64) {
	now := time.Now()
	elapsed := time.Duration(slot*params.BeaconConfig().SecondsPerSlot) * time.Second
	builder.service.SetGenesisTime(now.Add(-elapsed))
	builder.service.SetForkChoiceGenesisTime(now.Add(-elapsed))
}

func getInstruction(t *testing.T, slot uint64) slotInstruction {
	t.Helper()
	url := fmt.Sprintf("%s/fcr-sim/v1/slot/%d?warmup_start_slot=%d", baseURL(), slot, cli.warmupStartSlot)
	resp := get(t, url, "application/json")
	defer resp.Body.Close()
	var wire slotResponse
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		t.Fatalf("decode slot %d instruction: %v", slot, err)
	}
	if wire.Version != 3 || wire.Slot.SimSlot != slot {
		t.Fatalf("invalid slot instruction for %d: version=%d sim_slot=%d", slot, wire.Version, wire.Slot.SimSlot)
	}
	return wire.Slot
}

func getSSZ(t *testing.T, url string) []byte {
	t.Helper()
	resp := get(t, url, "application/octet-stream")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return body
}

func get(t *testing.T, url, accept string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("create request %s: %v", url, err)
	}
	req.Header.Set("Accept", accept)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		t.Fatalf("GET %s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp
}

func parseRoot(t *testing.T, value string) [32]byte {
	t.Helper()
	var root [32]byte
	decoded := parseHex(t, value)
	if len(decoded) != len(root) {
		t.Fatalf("invalid root %q", value)
	}
	copy(root[:], decoded)
	return root
}

func parseHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "0x"))
	if err != nil {
		t.Fatalf("invalid hex %q", value)
	}
	return decoded
}
