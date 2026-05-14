# Teku FCR Engine

Builds `fcr-teku` from a pinned Teku checkout.

```bash
bash engines/teku/build.sh
./results/fcr-teku --manifest-json
```

The script clones the submodule into `.build/teku`, applies `patches/*.patch`,
builds `:fcr-simulator-engine:shadowJar`, and writes `./results/fcr-teku`.
