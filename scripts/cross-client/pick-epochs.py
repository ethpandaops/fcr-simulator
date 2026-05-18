#!/usr/bin/env python3
"""Print 10 reproducibly-seeded random mainnet epochs to stdout, one per line."""
import argparse
import random

DEFAULT_SEED = 20260514
DEFAULT_LO = 435000
DEFAULT_HI = 445000
DEFAULT_N = 10


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--seed", type=int, default=DEFAULT_SEED)
    parser.add_argument("--lo", type=int, default=DEFAULT_LO)
    parser.add_argument("--hi", type=int, default=DEFAULT_HI)
    parser.add_argument("-n", type=int, default=DEFAULT_N)
    args = parser.parse_args()

    rng = random.Random(args.seed)
    epochs = sorted(rng.sample(range(args.lo, args.hi), args.n))
    for e in epochs:
        print(e)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
