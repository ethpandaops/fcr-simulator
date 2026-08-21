package blockchain

import (
	"github.com/OffchainLabs/prysm/v7/beacon-chain/operations/blstoexec"
	"github.com/OffchainLabs/prysm/v7/beacon-chain/operations/slashings"
	"github.com/OffchainLabs/prysm/v7/beacon-chain/operations/voluntaryexits"
)

// InitFCRSimulatorPools supplies operation pools omitted by Prysm's forkchoice
// spectest harness but needed when that harness replays full mainnet blocks.
func (s *Service) InitFCRSimulatorPools() {
	s.cfg.ExitPool = voluntaryexits.NewPool()
	s.cfg.SlashingPool = slashings.NewPool()
	s.cfg.BLSToExecPool = blstoexec.NewPool()
}
