package beaconapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHandleBlocksBySlot_Hit(t *testing.T) {
	backend := &fakeBackend{
		blocksBySlot: map[uint64][]byte{
			100: []byte("block-100"),
		},
		forkAtSlot: func(slot uint64) string {
			require.Equal(t, uint64(100), slot)
			return "capella"
		},
	}

	rec := doRequest(t, backend, "/eth/v2/beacon/blocks/100", contentTypeSSZ)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, contentTypeSSZ, rec.Header().Get("Content-Type"))
	require.Equal(t, "capella", rec.Header().Get("Eth-Consensus-Version"))
	require.Equal(t, "block-100", rec.Body.String())
}

func TestHandleBlocksBySlot_404(t *testing.T) {
	rec := doRequest(t, &fakeBackend{}, "/eth/v2/beacon/blocks/100", contentTypeSSZ)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandleBlocksByRoot_Hit(t *testing.T) {
	root := testRoot(0x11)
	backend := &fakeBackend{
		blocksByRoot: map[[32]byte][]byte{
			root: []byte("checkpoint-block"),
		},
	}

	rec := doRequest(t, backend, "/eth/v2/beacon/blocks/"+rootHex(root), contentTypeSSZ)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, contentTypeSSZ, rec.Header().Get("Content-Type"))
	require.Empty(t, rec.Header().Get("Eth-Consensus-Version"))
	require.Equal(t, "checkpoint-block", rec.Body.String())
}

func TestHandleBlocksByRoot_BadHexFormat(t *testing.T) {
	rec := doRequest(t, &fakeBackend{}, "/eth/v2/beacon/blocks/0x"+strings.Repeat("z", 64), contentTypeSSZ)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleStatesBySlot_Hit(t *testing.T) {
	backend := &fakeBackend{
		statesBySlot: map[uint64][]byte{
			100: []byte("state-100"),
		},
		forkAtSlot: func(slot uint64) string {
			require.Equal(t, uint64(100), slot)
			return "altair"
		},
	}

	rec := doRequest(t, backend, "/eth/v2/debug/beacon/states/100", contentTypeSSZ)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, contentTypeSSZ, rec.Header().Get("Content-Type"))
	require.Equal(t, "altair", rec.Header().Get("Eth-Consensus-Version"))
	require.Equal(t, "state-100", rec.Body.String())
}

func TestHandleStatesBySlot_404(t *testing.T) {
	rec := doRequest(t, &fakeBackend{}, "/eth/v2/debug/beacon/states/100", contentTypeSSZ)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandleStatesGenesis(t *testing.T) {
	backend := &fakeBackend{
		genesisState: []byte("genesis-state"),
		forkAtSlot:   MainnetForkAtSlot,
	}

	rec := doRequest(t, backend, "/eth/v2/debug/beacon/states/genesis", contentTypeSSZ)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, contentTypeSSZ, rec.Header().Get("Content-Type"))
	require.Equal(t, "phase0", rec.Header().Get("Eth-Consensus-Version"))
	require.Equal(t, "genesis-state", rec.Body.String())
}

func TestHandleGenesisInfo(t *testing.T) {
	backend := &fakeBackend{
		genesisInfo: GenesisInfo{
			GenesisTime:           1606824023,
			GenesisValidatorsRoot: "0x1234",
			GenesisForkVersion:    "0x00000000",
		},
	}

	rec := doRequest(t, backend, "/eth/v2/beacon/genesis", "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, contentTypeJSON, rec.Header().Get("Content-Type"))

	var got GenesisInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, backend.genesisInfo, got)
	require.Contains(t, rec.Body.String(), `"genesis_time":"1606824023"`)
}

func TestHandlePlan_Valid(t *testing.T) {
	source101 := uint64(101)
	source103 := uint64(103)
	backend := &fakeBackend{
		plan: []PlanEntry{
			{SimSlot: 100, SourceBlockSlot: &source101},
			{SimSlot: 101, SourceBlockSlot: nil},
			{SimSlot: 102, SourceBlockSlot: &source103},
			{SimSlot: 103, SourceBlockSlot: &source103},
		},
	}

	rec := doRequest(t, backend, "/fcr-sim/v1/plan?from=100&to=104", "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, contentTypeJSON, rec.Header().Get("Content-Type"))
	require.Equal(t, uint64(100), backend.planFrom)
	require.Equal(t, uint64(104), backend.planTo)

	var got struct {
		Version uint64      `json:"version"`
		Entries []PlanEntry `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, uint64(2), got.Version)
	require.Len(t, got.Entries, 4)
	require.Equal(t, uint64(100), got.Entries[0].SimSlot)
	require.NotNil(t, got.Entries[0].SourceBlockSlot)
	require.Equal(t, uint64(101), *got.Entries[0].SourceBlockSlot)
	require.Nil(t, got.Entries[1].SourceBlockSlot)
}

func TestHandlePlan_MissingFrom(t *testing.T) {
	rec := doRequest(t, &fakeBackend{}, "/fcr-sim/v1/plan?to=104", "")

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandlePlan_InvalidTo(t *testing.T) {
	rec := doRequest(t, &fakeBackend{}, "/fcr-sim/v1/plan?from=100&to=not-a-slot", "")

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandlePlan_FromGreaterThanTo(t *testing.T) {
	rec := doRequest(t, &fakeBackend{}, "/fcr-sim/v1/plan?from=105&to=104", "")

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAcceptHeaderMismatch(t *testing.T) {
	rec := doRequest(t, &fakeBackend{}, "/eth/v2/beacon/blocks/100", contentTypeJSON)

	require.Equal(t, http.StatusNotAcceptable, rec.Code)
}

func TestServeHTTP_MethodBackendAndRouteErrors(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/eth/v2/beacon/genesis", nil)
	rec := httptest.NewRecorder()
	NewServer(&fakeBackend{}).Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)

	rec = doRequest(t, nil, "/eth/v2/beacon/genesis", "")
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	rec = doRequest(t, &fakeBackend{}, "/unknown", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestBackendErrorsReturn500(t *testing.T) {
	tests := []string{
		"/eth/v2/beacon/blocks/100",
		"/eth/v2/debug/beacon/states/100",
		"/eth/v2/debug/beacon/states/genesis",
		"/eth/v2/beacon/genesis",
		"/fcr-sim/v1/plan?from=100&to=101",
	}

	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			rec := doRequest(t, &fakeBackend{err: errFakeBackend}, path, contentTypeSSZ)
			require.Equal(t, http.StatusInternalServerError, rec.Code)
		})
	}
}

func TestEthConsensusVersionHeader_SetCorrectly(t *testing.T) {
	backend := &fakeBackend{
		blocksBySlot: map[uint64][]byte{
			4636672: []byte("bellatrix-block"),
		},
		forkAtSlot: MainnetForkAtSlot,
	}

	rec := doRequest(t, backend, "/eth/v2/beacon/blocks/4636672", contentTypeSSZ)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "bellatrix", rec.Header().Get("Eth-Consensus-Version"))
}

func TestForkAtSlot_BoundaryTransitions(t *testing.T) {
	require.Equal(t, "phase0", MainnetForkAtSlot(0))
	require.Equal(t, "phase0", MainnetForkAtSlot(2375679))
	require.Equal(t, "altair", MainnetForkAtSlot(2375680))
	require.Equal(t, "altair", MainnetForkAtSlot(4636671))
	require.Equal(t, "bellatrix", MainnetForkAtSlot(4636672))
	require.Equal(t, "bellatrix", MainnetForkAtSlot(6209535))
	require.Equal(t, "capella", MainnetForkAtSlot(6209536))
	require.Equal(t, "capella", MainnetForkAtSlot(8626175))
	require.Equal(t, "deneb", MainnetForkAtSlot(8626176))
	require.Equal(t, "deneb", MainnetForkAtSlot(11649023))
	require.Equal(t, "electra", MainnetForkAtSlot(11649024))
}

func TestUnsupportedAndInvalidIDs(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		status int
	}{
		{name: "unsupported block id", path: "/eth/v2/beacon/blocks/head", status: http.StatusNotFound},
		{name: "wrong root length", path: "/eth/v2/beacon/blocks/0x1234", status: http.StatusBadRequest},
		{name: "invalid block id", path: "/eth/v2/beacon/blocks/not-a-slot", status: http.StatusBadRequest},
		{name: "state root unsupported", path: "/eth/v2/debug/beacon/states/0x1234", status: http.StatusNotFound},
		{name: "invalid state id", path: "/eth/v2/debug/beacon/states/head", status: http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, &fakeBackend{}, tc.path, contentTypeSSZ)
			require.Equal(t, tc.status, rec.Code)
		})
	}
}

func TestAcceptHeaderAllowsWildcardsAndOctetStream(t *testing.T) {
	backend := &fakeBackend{
		blocksBySlot: map[uint64][]byte{
			100: []byte("block-100"),
		},
		forkAtSlot: MainnetForkAtSlot,
	}

	tests := []string{"", "*/*", "application/octet-stream", "application/*;q=0.8"}
	for _, accept := range tests {
		t.Run(fmt.Sprintf("accept=%q", accept), func(t *testing.T) {
			rec := doRequest(t, backend, "/eth/v2/beacon/blocks/100", accept)
			require.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

func doRequest(t *testing.T, backend Backend, target string, accept string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, target, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	NewServer(backend).Handler().ServeHTTP(rec, req)
	return rec
}

type fakeBackend struct {
	blocksBySlot map[uint64][]byte
	blocksByRoot map[[32]byte][]byte
	statesBySlot map[uint64][]byte

	genesisState []byte
	genesisInfo  GenesisInfo

	plan     []PlanEntry
	planFrom uint64
	planTo   uint64

	forkAtSlot func(slot uint64) string

	err error
}

func (b *fakeBackend) BlockSSZBySlot(slot uint64) ([]byte, error) {
	if b.err != nil {
		return nil, b.err
	}
	data, ok := b.blocksBySlot[slot]
	if !ok {
		return nil, ErrNotFound
	}
	return data, nil
}

func (b *fakeBackend) BlockSSZByRoot(root [32]byte) ([]byte, error) {
	if b.err != nil {
		return nil, b.err
	}
	data, ok := b.blocksByRoot[root]
	if !ok {
		return nil, ErrNotFound
	}
	return data, nil
}

func (b *fakeBackend) StateSSZBySlot(slot uint64) ([]byte, error) {
	if b.err != nil {
		return nil, b.err
	}
	data, ok := b.statesBySlot[slot]
	if !ok {
		return nil, ErrNotFound
	}
	return data, nil
}

func (b *fakeBackend) GenesisStateSSZ() ([]byte, error) {
	if b.err != nil {
		return nil, b.err
	}
	if b.genesisState == nil {
		return nil, ErrNotFound
	}
	return b.genesisState, nil
}

func (b *fakeBackend) GenesisInfo() (GenesisInfo, error) {
	if b.err != nil {
		return GenesisInfo{}, b.err
	}
	return b.genesisInfo, nil
}

func (b *fakeBackend) ConsensusVersionAtSlot(slot uint64) string {
	if b.forkAtSlot == nil {
		return "phase0"
	}
	return b.forkAtSlot(slot)
}

func (b *fakeBackend) BuildPlan(from, to uint64) ([]PlanEntry, error) {
	if b.err != nil {
		return nil, b.err
	}
	b.planFrom = from
	b.planTo = to
	return b.plan, nil
}

var errFakeBackend = errors.New("fake backend error")

func testRoot(fill byte) [32]byte {
	var root [32]byte
	for i := range root {
		root[i] = fill
	}
	return root
}

func rootHex(root [32]byte) string {
	return "0x" + fmt.Sprintf("%x", root[:])
}
