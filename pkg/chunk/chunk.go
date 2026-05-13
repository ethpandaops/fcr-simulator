package chunk

import "math"

const SlotsPerEpoch uint64 = 32

// Chunk represents one worker's epoch range.
type Chunk struct {
	Index           int
	StartEpoch      uint64
	EndEpoch        uint64
	WarmupStartSlot uint64
	StartSlot       uint64
	EndSlot         uint64
}

// Split divides [startEpoch, endEpoch) into N evenly-sized chunks, with
// remainder distributed across earlier chunks (first `remainder` chunks
// get +1 epoch). Per-worker warmup epochs are subtracted from each chunk's
// start to give warmup_start. Warmup underflow is clamped to genesis slot 0.
func Split(startEpoch, endEpoch uint64, warmupEpochs uint64, parallel int) []Chunk {
	if parallel <= 0 || startEpoch >= endEpoch {
		return nil
	}

	total := endEpoch - startEpoch
	workers := uint64(parallel)
	base := total / workers
	remainder := total % workers

	chunks := make([]Chunk, 0, parallel)
	nextStart := startEpoch
	for i := 0; i < parallel; i++ {
		size := base
		if uint64(i) < remainder {
			size++
		}

		chunkStart := nextStart
		chunkEnd := chunkStart + size
		nextStart = chunkEnd

		warmupEpoch := uint64(0)
		if chunkStart > warmupEpochs {
			warmupEpoch = chunkStart - warmupEpochs
		}

		chunks = append(chunks, Chunk{
			Index:           i,
			StartEpoch:      chunkStart,
			EndEpoch:        chunkEnd,
			WarmupStartSlot: epochToSlot(warmupEpoch),
			StartSlot:       epochToSlot(chunkStart),
			EndSlot:         epochToSlot(chunkEnd),
		})
	}

	return chunks
}

func epochToSlot(epoch uint64) uint64 {
	if epoch > math.MaxUint64/SlotsPerEpoch {
		return math.MaxUint64
	}
	return epoch * SlotsPerEpoch
}
