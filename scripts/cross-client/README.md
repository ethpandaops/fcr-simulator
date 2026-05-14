# Cross-client smoke harness

Three pieces:

| Script | Purpose |
|---|---|
| `pick-epochs.py` | Deterministic mainnet epoch sample. Seed `20260514`, 10 epochs from `[435000, 445000)`. |
| `run.sh ENGINE BINARY` | Run the orchestrator over the 10 epochs for one engine. Writes `results/cross-client/<engine>/epoch-<N>.csv`. |
| `diff.py [engines...]` | Cross-engine per-slot diff. Writes `results/cross-client/per-slot.csv` + `divergences.csv` and prints a per-column disagreement summary. |

## Full run

```bash
go build -o results/fcr-orchestrator ./cmd/fcr-orchestrator
# Build each engine binary into results/fcr-<engine> per its own engine README.
scripts/cross-client/run.sh lighthouse ./results/fcr-lighthouse
scripts/cross-client/run.sh teku       ./results/fcr-teku
scripts/cross-client/run.sh lodestar   ./results/fcr-lodestar
scripts/cross-client/run.sh nimbus     ./results/fcr-nimbus
scripts/cross-client/run.sh grandine   ./results/fcr-grandine
# Prysm: see engines/prysm/README.md for algorithm-version caveats.
scripts/cross-client/diff.py
```

`run.sh` reads `BEACON_NODE_URL` (or legacy `BN_URL`) from `.env` (`set -a; source .env; set +a`); the beacon node must be in archive mode for older ranges.
