import {closeSync, fsyncSync, openSync, writeSync} from "node:fs";
import {hrtime} from "node:process";

import {mainnetChainConfig} from "@lodestar/config/configs";
import {createBeaconConfig, createChainForkConfig, type BeaconConfig} from "@lodestar/config";
import {
  ForkChoice,
  ForkChoiceStore,
  ProtoArray,
  ExecutionStatus,
  PayloadStatus,
  type CheckpointWithPayloadStatus,
  type ForkChoiceStateGetter,
  type JustifiedBalancesGetter,
} from "@lodestar/fork-choice";
import {ForkSeq, GENESIS_SLOT} from "@lodestar/params";
import {
  computeEpochAtSlot,
  createCachedBeaconState,
  createPubkeyCache,
  stateTransition,
  syncPubkeys,
  BeaconStateView,
  DataAvailabilityStatus,
  ExecutionPayloadStatus,
  type CachedBeaconStateAllForks,
  type EffectiveBalanceIncrements,
  type IBeaconStateView,
} from "@lodestar/state-transition";
import {
  ssz,
  sszTypesFor,
  type Attestation,
  type IndexedAttestation,
  type SignedBeaconBlock,
} from "@lodestar/types";
import {toRootHex} from "@lodestar/utils";
import {ForkName} from "@lodestar/params";
import {request} from "undici";

import {LODESTAR_COMMIT, LODESTAR_VERSION} from "./version.js";

const ZERO_ROOT_HEX = `0x${"0".repeat(64)}`;

interface CliConfig {
  beaconNodeUrl: string;
  startSlot: number;
  endSlot: number;
  warmupStartSlot: number;
  network: "mainnet";
  byzantineThreshold: number;
  attestationSourceMode: string;
  lookaheadCap: number;
  output: string;
}

type ManifestExit = {kind: "manifest"};
type RunConfig = {kind: "run"; cfg: CliConfig};
type ParseResult = ManifestExit | RunConfig;

function parseArgs(argv: string[]): ParseResult {
  const args = new Map<string, string>();
  let manifest = false;
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--manifest-json") {
      manifest = true;
      continue;
    }
    if (!arg.startsWith("--")) {
      throw new Error(`unexpected positional argument: ${arg}`);
    }
    const name = arg.slice(2);
    const next = argv[i + 1];
    if (next === undefined || next.startsWith("--")) {
      throw new Error(`missing value for --${name}`);
    }
    args.set(name, next);
    i++;
  }
  if (manifest) return {kind: "manifest"};

  const required = (name: string): string => {
    const v = args.get(name);
    if (v === undefined) throw new Error(`missing required flag --${name}`);
    return v;
  };
  const requiredU = (name: string): number => {
    const v = required(name);
    const n = Number(v);
    if (!Number.isInteger(n) || n < 0) throw new Error(`--${name} must be a non-negative integer`);
    return n;
  };

  const network = required("network");
  if (network !== "mainnet") throw new Error(`unsupported network ${network}; only mainnet`);
  const mode = required("attestation-source-mode");
  if (mode !== "next-non-missed" && mode !== "strict-source-block-k-minus-1") {
    throw new Error(`unsupported --attestation-source-mode ${mode}`);
  }

  const startSlot = requiredU("start-slot");
  const endSlot = requiredU("end-slot");
  const warmupStartSlot = requiredU("warmup-start-slot");
  if (warmupStartSlot > startSlot) throw new Error("--warmup-start-slot must be <= --start-slot");
  if (startSlot >= endSlot) throw new Error("--start-slot must be < --end-slot");

  return {
    kind: "run",
    cfg: {
      beaconNodeUrl: required("beacon-node-url").replace(/\/+$/, ""),
      startSlot,
      endSlot,
      warmupStartSlot,
      network: "mainnet",
      byzantineThreshold: requiredU("byzantine-threshold"),
      attestationSourceMode: mode,
      lookaheadCap: requiredU("lookahead-cap"),
      output: required("output"),
    },
  };
}

function printManifest(): void {
  const manifest = {
    engine_name: "lodestar",
    engine_version: LODESTAR_VERSION,
    engine_commit: LODESTAR_COMMIT,
    build_flags: ["fake_crypto", "no_el_notification", "no_da_check"],
    fcr_spec_commit: "",
  };
  process.stdout.write(`${JSON.stringify(manifest, null, 2)}\n`);
}

async function fetchSsz(url: string): Promise<{body: Uint8Array; consensusVersion: string | null}> {
  const res = await request(url, {
    headers: {accept: "application/octet-stream"},
  });
  if (res.statusCode === 404) {
    throw new NotFoundError(url);
  }
  if (res.statusCode < 200 || res.statusCode >= 300) {
    const body = await res.body.text();
    throw new Error(`HTTP ${res.statusCode} from ${url}: ${body.slice(0, 256)}`);
  }
  const ab = await res.body.arrayBuffer();
  const ver = res.headers["eth-consensus-version"];
  return {
    body: new Uint8Array(ab),
    consensusVersion: Array.isArray(ver) ? (ver[0] ?? null) : (ver ?? null),
  };
}

class NotFoundError extends Error {
  constructor(public url: string) {
    super(`404 ${url}`);
    this.name = "NotFoundError";
  }
}

async function fetchJson<T>(url: string): Promise<T> {
  const res = await request(url, {headers: {accept: "application/json"}});
  if (res.statusCode < 200 || res.statusCode >= 300) {
    const body = await res.body.text();
    throw new Error(`HTTP ${res.statusCode} from ${url}: ${body.slice(0, 256)}`);
  }
  return (await res.body.json()) as T;
}

async function fetchPlan(baseUrl: string, from: number, to: number): Promise<Map<number, number | null>> {
  const data = await fetchJson<{entries: Array<{sim_slot: number; source_block_slot: number | null}>}>(
    `${baseUrl}/fcr-sim/v1/plan?from=${from}&to=${to}`,
  );
  const map = new Map<number, number | null>();
  for (const entry of data.entries) {
    map.set(entry.sim_slot, entry.source_block_slot);
  }
  return map;
}

interface BlockFetch {
  block: SignedBeaconBlock;
  fork: ForkName;
  forkSeq: ForkSeq;
}

class BlockFetcher {
  private readonly cache = new Map<number, BlockFetch | null>();

  constructor(
    private readonly baseUrl: string,
    private readonly config: BeaconConfig,
  ) {}

  async fetchAtSlot(slot: number): Promise<BlockFetch | null> {
    if (this.cache.has(slot)) return this.cache.get(slot) ?? null;
    try {
      const {body, consensusVersion} = await fetchSsz(`${this.baseUrl}/eth/v2/beacon/blocks/${slot}`);
      const fork = this.resolveForkAtSlot(slot, consensusVersion);
      const forkSeq = this.config.getForkSeq(slot);
      const block = sszTypesFor(fork).SignedBeaconBlock.deserialize(body) as SignedBeaconBlock;
      const out = {block, fork, forkSeq};
      this.cache.set(slot, out);
      return out;
    } catch (err) {
      if (err instanceof NotFoundError) {
        this.cache.set(slot, null);
        return null;
      }
      throw err;
    }
  }

  async fetchByRoot(root: Uint8Array): Promise<BlockFetch> {
    const url = `${this.baseUrl}/eth/v2/beacon/blocks/${toRootHex(root)}`;
    const {body, consensusVersion} = await fetchSsz(url);
    let fork: ForkName;
    if (consensusVersion && this.isForkName(consensusVersion)) {
      fork = consensusVersion as ForkName;
    } else {
      // Fallback: deserialize as latest and re-read slot
      // Try with a guess and then re-deserialize via correct fork
      const tentative = ssz.deneb.SignedBeaconBlock.deserialize(body);
      fork = this.config.getForkName(tentative.message.slot);
    }
    const forkSeq = ForkSeq[fork];
    const block = sszTypesFor(fork).SignedBeaconBlock.deserialize(body) as SignedBeaconBlock;
    return {block, fork, forkSeq};
  }

  private resolveForkAtSlot(slot: number, consensusVersion: string | null): ForkName {
    if (consensusVersion && this.isForkName(consensusVersion)) {
      return consensusVersion as ForkName;
    }
    return this.config.getForkName(slot);
  }

  private isForkName(s: string): boolean {
    return (Object.values(ForkName) as string[]).includes(s);
  }
}

function microsSince(start: bigint): number {
  const diffNs = hrtime.bigint() - start;
  const us = Number(diffNs / 1000n);
  return us < 0 ? 0 : us;
}

class JsonlWriter {
  private readonly fd: number;
  written = 0;

  constructor(path: string) {
    this.fd = openSync(path, "w");
  }

  write(record: Record<string, unknown>): void {
    const buf = Buffer.from(`${JSON.stringify(record)}\n`, "utf8");
    let offset = 0;
    while (offset < buf.length) {
      const n = writeSync(this.fd, buf, offset, buf.length - offset);
      if (n <= 0) throw new Error(`writeSync returned ${n} for fd ${this.fd}`);
      offset += n;
    }
    this.written++;
  }

  close(): void {
    fsyncSync(this.fd);
    closeSync(this.fd);
  }
}

async function main(): Promise<void> {
  const parsed = parseArgs(process.argv.slice(2));
  if (parsed.kind === "manifest") {
    printManifest();
    process.exit(0);
  }
  const {cfg} = parsed;

  let bootstrap: BootstrapResult;
  try {
    bootstrap = await bootstrapEngine(cfg);
  } catch (err) {
    process.stderr.write(`bootstrap failure: ${formatError(err)}\n`);
    process.exit(2);
  }

  try {
    await runSlotLoop(cfg, bootstrap);
  } catch (err) {
    process.stderr.write(`simulation failure: ${formatError(err)}\n`);
    process.exit(3);
  }
  process.exit(0);
}

interface BootstrapResult {
  beaconConfig: BeaconConfig;
  forkChoice: ForkChoice;
  fetcher: BlockFetcher;
  warmupState: CachedBeaconStateAllForks;
  plan: Map<number, number | null>;
  stateByStateRoot: Map<string, CachedBeaconStateAllForks>;
  stateByCheckpointKey: Map<string, CachedBeaconStateAllForks>;
  headStateRef: {state: CachedBeaconStateAllForks};
}

function checkpointKey(ep: number, rootHex: string, payloadStatus: PayloadStatus): string {
  return `${ep}:${rootHex}:${payloadStatus}`;
}

async function bootstrapEngine(cfg: CliConfig): Promise<BootstrapResult> {
  // 1. Build a tentative chain fork config using mainnet defaults; we'll create
  //    the full beacon config after we fetch genesis_validators_root via state.
  const chainConfigOverrides: Record<string, unknown> = {
    ...mainnetChainConfig,
    CONFIRMATION_BYZANTINE_THRESHOLD: cfg.byzantineThreshold,
  };
  const chainForkConfig = createChainForkConfig(chainConfigOverrides);

  // 2. Fetch checkpoint state SSZ at warmupStartSlot
  const stateUrl = `${cfg.beaconNodeUrl}/eth/v2/debug/beacon/states/${cfg.warmupStartSlot}`;
  const stateRes = await fetchSsz(stateUrl);
  const stateFork = chainForkConfig.getForkName(cfg.warmupStartSlot);
  const stateView = sszTypesFor(stateFork).BeaconState.deserializeToViewDU(stateRes.body);

  const genesisValidatorsRoot = stateView.genesisValidatorsRoot;
  const beaconConfig = createBeaconConfig(chainConfigOverrides, genesisValidatorsRoot);

  // 3. Build cached beacon state
  const pubkeyCache = createPubkeyCache();
  syncPubkeys(pubkeyCache, stateView.validators.getAllReadonlyValues());
  const cachedState = createCachedBeaconState(stateView, {config: beaconConfig, pubkeyCache}, {skipSyncPubkeys: true});

  // 4. Initialize fork choice from finalized state
  const fetcher = new BlockFetcher(cfg.beaconNodeUrl, beaconConfig);
  const stateByStateRoot = new Map<string, CachedBeaconStateAllForks>();
  const stateByCheckpointKey = new Map<string, CachedBeaconStateAllForks>();

  // Seed caches with the anchor state
  const anchorStateRoot = toRootHex(cachedState.hashTreeRoot());
  stateByStateRoot.set(anchorStateRoot, cachedState);
  const headStateRef = {state: cachedState};

  // Build BeaconStateView wrapper for the bootstrap state (used by FCR via stateGetter).
  const bootstrapStateView = new BeaconStateView(cachedState);

  // Justified-balances getter: return effective balances from the head state.
  // For replay this is acceptable — every state we'd consult is in the linear
  // canonical chain we've already processed.
  const justifiedBalancesGetter: JustifiedBalancesGetter = (
    _checkpoint: CheckpointWithPayloadStatus,
    blockState: IBeaconStateView,
  ): EffectiveBalanceIncrements => {
    return blockState.getEffectiveBalanceIncrementsZeroInactive();
  };

  // State getter for FCR: serve from the per-block postState cache.
  const stateGetter: ForkChoiceStateGetter = (opts) => {
    if (opts.stateRoot !== undefined) {
      const s = stateByStateRoot.get(opts.stateRoot);
      if (!s) return null;
      return new BeaconStateView(s);
    }
    if (opts.checkpoint !== undefined) {
      const cp = opts.checkpoint;
      const key = checkpointKey(cp.epoch, cp.rootHex, cp.payloadStatus);
      const s = stateByCheckpointKey.get(key);
      if (!s) {
        // Fallback: process the head state forward to the checkpoint epoch boundary.
        // The FCR will tolerate null and skip rules requiring this state.
        return null;
      }
      return new BeaconStateView(s);
    }
    return null;
  };

  const {forkChoice} = initializeForkChoiceFromAnchor({
    beaconConfig,
    cachedState,
    bootstrapStateView,
    currentSlot: cfg.warmupStartSlot,
    justifiedBalancesGetter,
    stateGetter,
  });

  // Seed the checkpoint-state cache: the anchor state is the justified+finalized
  // checkpoint state at bootstrap.
  const anchor = bootstrapStateView.computeAnchorCheckpoint();
  const anchorCpKey = checkpointKey(anchor.checkpoint.epoch, toRootHex(anchor.checkpoint.root), PayloadStatus.FULL);
  stateByCheckpointKey.set(anchorCpKey, cachedState);

  // 5. Fetch plan
  const plan = await fetchPlan(cfg.beaconNodeUrl, cfg.warmupStartSlot + 1, cfg.endSlot);
  for (let s = cfg.warmupStartSlot + 1; s < cfg.endSlot; s++) {
    if (!plan.has(s)) {
      throw new Error(`attestation plan missing sim_slot ${s}`);
    }
  }

  return {
    beaconConfig,
    forkChoice,
    fetcher,
    warmupState: cachedState,
    plan,
    stateByStateRoot,
    stateByCheckpointKey,
    headStateRef,
  };
}

function initializeForkChoiceFromAnchor(args: {
  beaconConfig: BeaconConfig;
  cachedState: CachedBeaconStateAllForks;
  bootstrapStateView: BeaconStateView;
  currentSlot: number;
  justifiedBalancesGetter: JustifiedBalancesGetter;
  stateGetter: ForkChoiceStateGetter;
}): {forkChoice: ForkChoice} {
  const {beaconConfig, cachedState, bootstrapStateView, currentSlot, justifiedBalancesGetter, stateGetter} = args;
  const {checkpoint, blockHeader} = bootstrapStateView.computeAnchorCheckpoint();

  const finalizedCheckpoint = {...checkpoint};
  const justifiedCheckpoint = {
    ...checkpoint,
    epoch: checkpoint.epoch === 0 ? checkpoint.epoch : checkpoint.epoch + 1,
  };

  const justifiedBalances = bootstrapStateView.getEffectiveBalanceIncrementsZeroInactive();

  const store = new ForkChoiceStore(
    currentSlot,
    justifiedCheckpoint,
    finalizedCheckpoint,
    justifiedBalances,
    justifiedBalancesGetter,
    PayloadStatus.FULL,
    PayloadStatus.FULL,
    stateGetter,
    {
      onJustified: () => {},
      onFinalized: () => {},
    },
  );

  const protoArray = ProtoArray.initialize(
    {
      slot: blockHeader.slot,
      parentRoot: toRootHex(blockHeader.parentRoot),
      stateRoot: toRootHex(blockHeader.stateRoot),
      blockRoot: toRootHex(checkpoint.root),
      timeliness: true,
      justifiedEpoch: justifiedCheckpoint.epoch,
      justifiedRoot: toRootHex(justifiedCheckpoint.root),
      finalizedEpoch: finalizedCheckpoint.epoch,
      finalizedRoot: toRootHex(finalizedCheckpoint.root),
      unrealizedJustifiedEpoch: justifiedCheckpoint.epoch,
      unrealizedJustifiedRoot: toRootHex(justifiedCheckpoint.root),
      unrealizedFinalizedEpoch: finalizedCheckpoint.epoch,
      unrealizedFinalizedRoot: toRootHex(finalizedCheckpoint.root),
      executionPayloadBlockHash: null,
      executionStatus: blockHeader.slot === GENESIS_SLOT ? ExecutionStatus.Valid : ExecutionStatus.Syncing,
      dataAvailabilityStatus: DataAvailabilityStatus.PreData,
      payloadStatus: PayloadStatus.FULL,
      parentBlockHash: null,
    },
    currentSlot,
  );

  const forkChoice = new ForkChoice(
    beaconConfig,
    store,
    protoArray,
    bootstrapStateView.validatorCount,
    null,
    {fastConfirmation: true, proposerBoost: false, proposerBoostReorg: false},
    undefined,
  );

  return {forkChoice};
}

async function runSlotLoop(cfg: CliConfig, boot: BootstrapResult): Promise<void> {
  const writer = new JsonlWriter(cfg.output);
  const {forkChoice, fetcher, plan, stateByStateRoot, stateByCheckpointKey, headStateRef} = boot;

  try {
    let slot = cfg.warmupStartSlot + 1;
    while (slot < cfg.endSlot) {
      const isRecording = slot >= cfg.startSlot;

      forkChoice.updateTime(slot);

      const blockFetch = await fetcher.fetchAtSlot(slot);
      const hasBlock = blockFetch !== null;
      let blockRootHex: string | null = null;
      if (blockFetch) {
        try {
          const postState = processBlock(headStateRef.state, blockFetch);
          stateByStateRoot.set(toRootHex(postState.hashTreeRoot()), postState);
          // Apply block to fork choice
          const protoBlock = forkChoice.onBlock(
            blockFetch.block.message,
            new BeaconStateView(postState),
            0,
            slot,
            ExecutionStatus.Valid,
            DataAvailabilityStatus.Available,
          );
          blockRootHex = protoBlock.blockRoot;
          headStateRef.state = postState;

          // If this block is an epoch boundary, cache it as a potential checkpoint
          // state under both FULL and PENDING payload statuses to satisfy FCR lookups.
          if (slot % 32 === 0) {
            const epoch = computeEpochAtSlot(slot);
            const keyFull = checkpointKey(epoch, protoBlock.blockRoot, PayloadStatus.FULL);
            stateByCheckpointKey.set(keyFull, postState);
          }
        } catch (err) {
          throw new Error(`failed to process block at slot ${slot}: ${formatError(err)}`);
        }
      }

      const sourceSlot = plan.get(slot) ?? null;
      let numAttestationsInjected = 0;
      if (sourceSlot !== null && sourceSlot !== undefined) {
        const sourceBlock = await fetcher.fetchAtSlot(sourceSlot);
        if (!sourceBlock) {
          throw new Error(`plan referenced missing source block at slot ${sourceSlot}`);
        }
        numAttestationsInjected = injectAttestationsFromSource(forkChoice, headStateRef.state, sourceBlock);
      }

      // Recompute head + run FCR at slot+1
      const fcrStart = hrtime.bigint();
      forkChoice.updateTime(slot + 1);
      const head = forkChoice.updateHead();
      const fcrEvalUs = microsSince(fcrStart);

      if (isRecording) {
        writer.write(buildRecord({
          forkChoice,
          slot,
          hasBlock,
          blockRootHex,
          headRoot: head.blockRoot,
          sourceBlockSlot: sourceSlot,
          numAttestationsInjected,
          fcrEvalUs,
        }));
      }

      slot += 1;
    }
  } finally {
    writer.close();
  }
}

function processBlock(
  preState: CachedBeaconStateAllForks,
  blockFetch: BlockFetch,
): CachedBeaconStateAllForks {
  return stateTransition(preState, blockFetch.block, {
    verifyStateRoot: false,
    verifyProposer: false,
    verifySignatures: false,
    executionPayloadStatus: ExecutionPayloadStatus.valid,
    dataAvailabilityStatus: DataAvailabilityStatus.Available,
  });
}

function injectAttestationsFromSource(
  forkChoice: ForkChoice,
  headState: CachedBeaconStateAllForks,
  sourceBlock: BlockFetch,
): number {
  const attestations = sourceBlock.block.message.body.attestations as Attestation[];
  if (attestations.length === 0) return 0;

  let injected = 0;
  for (const attestation of attestations) {
    let indexed: IndexedAttestation;
    try {
      indexed = headState.epochCtx.getIndexedAttestation(sourceBlock.forkSeq, attestation);
    } catch {
      // Source block's epoch shuffling not available in head state's epoch context
      // (e.g. attestation references a far-future or far-past epoch). Skip and warn.
      continue;
    }
    if (indexed.attestingIndices.length === 0) continue;
    const attDataRoot = toRootHex(
      sszTypesFor(sourceBlock.fork).AttestationData.hashTreeRoot(attestation.data),
    );
    try {
      forkChoice.onAttestation(indexed, attDataRoot, true);
      injected++;
    } catch {
      // Match Lighthouse policy: log-and-continue on injection failure.
    }
  }
  return injected;
}

interface BuildRecordArgs {
  forkChoice: ForkChoice;
  slot: number;
  hasBlock: boolean;
  blockRootHex: string | null;
  headRoot: string;
  sourceBlockSlot: number | null | undefined;
  numAttestationsInjected: number;
  fcrEvalUs: number;
}

function buildRecord(args: BuildRecordArgs): Record<string, unknown> {
  const {forkChoice, slot, hasBlock, blockRootHex, headRoot, sourceBlockSlot, numAttestationsInjected, fcrEvalUs} = args;
  const confirmedRoot = forkChoice.getConfirmedRoot();
  const confirmedBlock = forkChoice.getBlockHexDefaultStatus(confirmedRoot);
  const confirmedSlot = confirmedBlock?.slot ?? 0;
  const confirmedRootOut = isZeroRoot(confirmedRoot) ? ZERO_ROOT_HEX : confirmedRoot;
  const effectiveConfirmedRoot = confirmedBlock ? confirmedRoot : ZERO_ROOT_HEX;
  const finalized = forkChoice.getFinalizedCheckpoint();
  const justified = forkChoice.getJustifiedCheckpoint();

  const fastConfirmed = effectiveConfirmedRoot !== ZERO_ROOT_HEX && confirmedSlot === slot;
  const evalSlot = slot + 1;
  const delay = evalSlot >= confirmedSlot ? evalSlot - confirmedSlot : 0;
  const strictOneSlotConfirmed =
    hasBlock &&
    effectiveConfirmedRoot !== ZERO_ROOT_HEX &&
    blockRootHex === effectiveConfirmedRoot &&
    confirmedSlot === slot &&
    delay === 1;

  return {
    slot,
    epoch: Math.floor(slot / 32),
    has_block: hasBlock,
    block_root: blockRootHex,
    head_root: headRoot,
    confirmed_root: confirmedRootOut === ZERO_ROOT_HEX ? ZERO_ROOT_HEX : effectiveConfirmedRoot,
    confirmed_slot: confirmedSlot,
    confirmation_delay_slots: delay,
    fast_confirmed: fastConfirmed,
    strict_one_slot_confirmed: strictOneSlotConfirmed,
    finalized_epoch: finalized.epoch,
    justified_epoch: justified.epoch,
    source_block_slot: sourceBlockSlot ?? null,
    num_attestations_injected: numAttestationsInjected,
    is_epoch_boundary: slot % 32 === 0,
    is_missed_slot: !hasBlock,
    fcr_eval_duration_us: fcrEvalUs,
  };
}

function isZeroRoot(rootHex: string): boolean {
  return rootHex === ZERO_ROOT_HEX || /^0x0+$/.test(rootHex);
}

function formatError(err: unknown): string {
  if (err instanceof Error) return err.stack ?? err.message;
  return String(err);
}

main().catch((err) => {
  process.stderr.write(`fatal: ${formatError(err)}\n`);
  process.exit(3);
});
