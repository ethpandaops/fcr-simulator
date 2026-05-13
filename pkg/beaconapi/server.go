package beaconapi

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const (
	contentTypeSSZ  = "application/octet-stream"
	contentTypeJSON = "application/json"

	blocksPrefix = "/eth/v2/beacon/blocks/"
	statesPrefix = "/eth/v2/debug/beacon/states/"
	genesisPath  = "/eth/v2/beacon/genesis"
	planPath     = "/fcr-sim/v1/plan"
)

// Server is the HTTP server.
type Server struct {
	backend Backend
}

func NewServer(b Backend) *Server {
	return &Server{backend: b}
}

// Handler returns the http.Handler for net/http integration.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.backend == nil {
		http.Error(w, "backend is not configured", http.StatusInternalServerError)
		return
	}

	switch {
	case strings.HasPrefix(r.URL.Path, blocksPrefix):
		s.handleBlock(w, r)
	case strings.HasPrefix(r.URL.Path, statesPrefix):
		s.handleState(w, r)
	case r.URL.Path == genesisPath:
		s.handleGenesis(w, r)
	case r.URL.Path == planPath:
		s.handlePlan(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleBlock(w http.ResponseWriter, r *http.Request) {
	if !acceptsSSZ(r.Header.Get("Accept")) {
		http.Error(w, "SSZ response is not acceptable", http.StatusNotAcceptable)
		return
	}

	blockID := strings.TrimPrefix(r.URL.Path, blocksPrefix)
	if blockID == "" || strings.Contains(blockID, "/") {
		http.NotFound(w, r)
		return
	}

	parsed, err := parseBlockID(blockID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	switch parsed.kind {
	case blockIDSlot:
		data, err := s.backend.BlockSSZBySlot(parsed.slot)
		if err != nil {
			writeBackendError(w, err)
			return
		}
		writeSSZ(w, data, s.backend.ConsensusVersionAtSlot(parsed.slot))
	case blockIDRoot:
		data, err := s.backend.BlockSSZByRoot(parsed.root)
		if err != nil {
			writeBackendError(w, err)
			return
		}
		// V1 does not set Eth-Consensus-Version for root-based block fetches:
		// the backend contract does not expose the checkpoint block slot.
		writeSSZ(w, data, "")
	case blockIDUnsupported:
		http.NotFound(w, r)
	default:
		http.Error(w, "invalid block id", http.StatusBadRequest)
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if !acceptsSSZ(r.Header.Get("Accept")) {
		http.Error(w, "SSZ response is not acceptable", http.StatusNotAcceptable)
		return
	}

	stateID := strings.TrimPrefix(r.URL.Path, statesPrefix)
	if stateID == "" || strings.Contains(stateID, "/") {
		http.NotFound(w, r)
		return
	}

	if stateID == "genesis" {
		data, err := s.backend.GenesisStateSSZ()
		if err != nil {
			writeBackendError(w, err)
			return
		}
		writeSSZ(w, data, s.backend.ConsensusVersionAtSlot(0))
		return
	}

	if strings.HasPrefix(stateID, "0x") {
		http.NotFound(w, r)
		return
	}

	slot, ok := parseUint64ID(stateID)
	if !ok {
		http.Error(w, "invalid state id", http.StatusBadRequest)
		return
	}

	data, err := s.backend.StateSSZBySlot(slot)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeSSZ(w, data, s.backend.ConsensusVersionAtSlot(slot))
}

func (s *Server) handleGenesis(w http.ResponseWriter, _ *http.Request) {
	info, err := s.backend.GenesisInfo()
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, info)
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	from, ok := parseRequiredUint64Query(query.Get("from"))
	if !ok {
		http.Error(w, "from query parameter is required and must be a uint64", http.StatusBadRequest)
		return
	}
	to, ok := parseRequiredUint64Query(query.Get("to"))
	if !ok {
		http.Error(w, "to query parameter is required and must be a uint64", http.StatusBadRequest)
		return
	}
	if from > to {
		http.Error(w, "from must be less than or equal to to", http.StatusBadRequest)
		return
	}

	entries, err := s.backend.BuildPlan(from, to)
	if err != nil {
		writeBackendError(w, err)
		return
	}

	writeJSON(w, struct {
		Entries []PlanEntry `json:"entries"`
	}{Entries: entries})
}

type blockIDKind int

const (
	blockIDInvalid blockIDKind = iota
	blockIDSlot
	blockIDRoot
	blockIDUnsupported
)

type parsedBlockID struct {
	kind blockIDKind
	slot uint64
	root [32]byte
}

func parseBlockID(value string) (parsedBlockID, error) {
	switch value {
	case "genesis", "head", "finalized", "justified":
		return parsedBlockID{kind: blockIDUnsupported}, nil
	}

	if slot, ok := parseUint64ID(value); ok {
		return parsedBlockID{kind: blockIDSlot, slot: slot}, nil
	}

	if strings.HasPrefix(value, "0x") {
		if len(value) != 66 {
			return parsedBlockID{}, fmt.Errorf("invalid block root length")
		}

		decoded, err := hex.DecodeString(value[2:])
		if err != nil {
			return parsedBlockID{}, fmt.Errorf("invalid block root hex")
		}
		if len(decoded) != 32 {
			return parsedBlockID{}, fmt.Errorf("invalid block root length")
		}

		var root [32]byte
		copy(root[:], decoded)
		return parsedBlockID{kind: blockIDRoot, root: root}, nil
	}

	return parsedBlockID{kind: blockIDInvalid}, fmt.Errorf("invalid block id")
}

func parseUint64ID(value string) (uint64, bool) {
	if value == "" {
		return 0, false
	}
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return 0, false
		}
	}

	parsed, err := strconv.ParseUint(value, 10, 64)
	return parsed, err == nil
}

func parseRequiredUint64Query(value string) (uint64, bool) {
	if value == "" {
		return 0, false
	}
	return parseUint64ID(value)
}

func acceptsSSZ(accept string) bool {
	accept = strings.TrimSpace(accept)
	if accept == "" {
		return true
	}

	for _, item := range strings.Split(accept, ",") {
		mediaType, q := parseAcceptItem(item)
		if q == "0" || q == "0.0" || q == "0.00" || q == "0.000" {
			continue
		}
		switch mediaType {
		case contentTypeSSZ, "application/*", "*/*":
			return true
		}
	}

	return false
}

func parseAcceptItem(item string) (mediaType string, q string) {
	q = "1"
	parts := strings.Split(item, ";")
	mediaType = strings.ToLower(strings.TrimSpace(parts[0]))
	for _, part := range parts[1:] {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.EqualFold(key, "q") {
			q = strings.TrimSpace(value)
		}
	}
	return mediaType, q
}

func writeSSZ(w http.ResponseWriter, data []byte, consensusVersion string) {
	if consensusVersion != "" {
		w.Header().Set("Eth-Consensus-Version", consensusVersion)
	}
	w.Header().Set("Content-Type", contentTypeSSZ)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(value)
}

func writeBackendError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
