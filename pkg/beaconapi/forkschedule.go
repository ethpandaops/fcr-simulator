package beaconapi

// ForkSchedule returns the active fork name at a given slot.
type ForkSchedule struct {
	SlotFork func(slot uint64) string
}

const (
	mainnetAltairSlot    uint64 = 2375680
	mainnetBellatrixSlot uint64 = 4636672
	mainnetCapellaSlot   uint64 = 6209536
	mainnetDenebSlot     uint64 = 8626176
	mainnetElectraSlot   uint64 = 11649024
)

func MainnetForkAtSlot(slot uint64) string {
	switch {
	case slot >= mainnetElectraSlot:
		return "electra"
	case slot >= mainnetDenebSlot:
		return "deneb"
	case slot >= mainnetCapellaSlot:
		return "capella"
	case slot >= mainnetBellatrixSlot:
		return "bellatrix"
	case slot >= mainnetAltairSlot:
		return "altair"
	default:
		return "phase0"
	}
}
