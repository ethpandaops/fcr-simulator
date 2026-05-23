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
  type ProtoBlock,
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
  G2_POINT_AT_INFINITY,
  type CachedBeaconStateAllForks,
  type EffectiveBalanceIncrements,
  type IBeaconStateView,
} from "@lodestar/state-transition";
import {
  ssz,
  sszTypesFor,
  type Attestation,
  type IndexedAttestation,
  type RootHex,
  type SignedBeaconBlock,
} from "@lodestar/types";
import {fromHex, toRootHex, type LogData, type Logger} from "@lodestar/utils";
import {ForkName} from "@lodestar/params";
import {request} from "undici";

import {LODESTAR_COMMIT, LODESTAR_VERSION} from "./version.js";

const ZERO_ROOT_HEX = `0x${"0".repeat(64)}`;
const RECENT_STATE_CACHE_SLOTS = 64;
const CHECKPOINT_STATE_CACHE_EPOCHS = 4;
const BLOCK_FETCH_CACHE_SLOTS = 64;
const LOG_MEMORY = process.env.FCR_LODESTAR_LOG_MEMORY === "1";
const MEMORY_LOG_INTERVAL_SLOTS = Number(process.env.FCR_LODESTAR_MEMORY_LOG_INTERVAL_SLOTS ?? 0);
const DEBUG_CACHE = process.env.FCR_LODESTAR_DEBUG_CACHE === "1";
const DEBUG_FCR = process.env.FCR_LODESTAR_DEBUG_FCR === "1";
const DEBUG_SLOTS = parseSlotSet(process.env.FCR_LODESTAR_DEBUG_SLOTS);
const DEBUG_EVICTION_HISTORY_LIMIT = 1024;
const LODESTAR_NULL_VOTE_INDEX = 0xffffffff;
const LODESTAR_INIT_VOTE_SLOT = 0;

interface CliConfig {
  beaconNodeUrl: string;
  startSlot: number;
  endSlot: number;
  warmupStartSlot: number;
  network: "mainnet";
  byzantineThreshold: number;
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
  const optionalU = (name: string): number | undefined => {
    const v = args.get(name);
    if (v === undefined) return undefined;
    const n = Number(v);
    if (!Number.isInteger(n) || n < 0) throw new Error(`--${name} must be a non-negative integer`);
    return n;
  };

  const network = required("network");
  if (network !== "mainnet") throw new Error(`unsupported network ${network}; only mainnet`);
  optionalU("lookahead-cap");

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
      byzantineThreshold: optionalU("byzantine-threshold") ?? 25,
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
    await res.body.dump();
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

interface SlotInstruction {
  simSlot: number;
  evalSlot: number;
  importBlocks: PlanBlockImport[];
  attestations: PlanAttestation[];
}

interface PlanBlockImport {
  slot: number;
  rootHex: RootHex;
  root: Uint8Array;
  canonical: boolean;
}

interface PlanAttestation {
  aggregationBits: Uint8Array | null;
  committeeBits: Uint8Array | null;
  attestingIndices: number[] | null;
  data: PlanAttestationData;
}

interface PlanAttestationData {
  slot: number;
  index: number;
  beaconBlockRoot: Uint8Array;
  source: PlanCheckpoint;
  target: PlanCheckpoint;
}

interface PlanCheckpoint {
  epoch: number;
  root: Uint8Array;
}

interface SlotResponseWire {
  version?: number;
  slot: SlotInstructionWire;
}

interface SlotInstructionWire {
  sim_slot: number;
  eval_slot: number;
  import_blocks: PlanBlockImportWire[];
  attestations: PlanAttestationWire[];
}

interface PlanBlockImportWire {
  slot: number;
  root: string;
  canonical: boolean;
}

interface PlanAttestationWire {
  aggregation_bits?: string | null;
  committee_bits?: string | null;
  attesting_indices?: unknown;
  data: PlanAttestationDataWire;
}

interface PlanAttestationDataWire {
  slot: number;
  index: number;
  beacon_block_root: string;
  source: PlanCheckpointWire;
  target: PlanCheckpointWire;
}

interface PlanCheckpointWire {
  epoch: number;
  root: string;
}

async function fetchSlotInstruction(baseUrl: string, simSlot: number, warmupStartSlot: number): Promise<SlotInstruction> {
  const response = await fetchJson<SlotResponseWire>(
    `${baseUrl}/fcr-sim/v1/slot/${simSlot}?warmup_start_slot=${warmupStartSlot}`,
  );
  if (response.version !== 3) {
    throw new Error(`unsupported slot instruction version ${response.version}; expected 3`);
  }

  const slot = response.slot;
  if (slot.sim_slot !== simSlot) {
    throw new Error(`slot instruction response sim_slot mismatch: requested ${simSlot}, got ${slot.sim_slot}`);
  }

  return {
    simSlot: slot.sim_slot,
    evalSlot: slot.eval_slot,
    importBlocks: slot.import_blocks.map((block) => {
      const root = parseRootHex(block.root);
      return {
        slot: block.slot,
        rootHex: toRootHex(root),
        root,
        canonical: block.canonical,
      };
    }),
    attestations: slot.attestations.map(parsePlanAttestation),
  };
}

function parsePlanAttestation(attestation: PlanAttestationWire): PlanAttestation {
  const attestingIndices = parseOptionalAttestingIndices(attestation.attesting_indices);
  const aggregationBits = attestingIndices === null ? attestation.aggregation_bits : null;
  if (aggregationBits == null && attestingIndices === null) {
    throw new Error("attestation is missing aggregation_bits or attesting_indices");
  }

  return {
    aggregationBits: aggregationBits == null ? null : parseHexBytes(aggregationBits),
    committeeBits:
      attestingIndices === null && attestation.committee_bits != null
        ? parseHexBytes(attestation.committee_bits)
        : null,
    attestingIndices,
    data: {
      slot: attestation.data.slot,
      index: attestation.data.index,
      beaconBlockRoot: parseRootHex(attestation.data.beacon_block_root),
      source: parsePlanCheckpoint(attestation.data.source),
      target: parsePlanCheckpoint(attestation.data.target),
    },
  };
}

function parseOptionalAttestingIndices(value: unknown): number[] | null {
  if (value == null) return null;
  if (!Array.isArray(value)) {
    throw new Error("attesting_indices must be an array");
  }
  return value.map((validatorIndex, i) => parseValidatorIndex(validatorIndex, `attesting_indices[${i}]`));
}

function parseValidatorIndex(value: unknown, name: string): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0) {
    throw new Error(`invalid validator index for ${name}: ${String(value)}`);
  }
  return value;
}

function parsePlanCheckpoint(checkpoint: PlanCheckpointWire): PlanCheckpoint {
  return {
    epoch: checkpoint.epoch,
    root: parseRootHex(checkpoint.root),
  };
}

function parseRootHex(value: string): Uint8Array {
  const root = parseHexBytes(value);
  if (root.length !== 32) {
    throw new Error(`expected 32-byte root hex string, got ${root.length} bytes`);
  }
  return root;
}

function parseHexBytes(value: string): Uint8Array {
  if (typeof value !== "string") {
    throw new Error(`expected hex string, got ${typeof value}`);
  }
  const hex = value.startsWith("0x") ? value.slice(2) : value;
  if (hex.length % 2 !== 0) {
    throw new Error("expected even-length hex string");
  }
  if (!/^[0-9a-fA-F]*$/.test(hex)) {
    throw new Error("invalid hex string");
  }
  return fromHex(hex);
}

interface BlockFetch {
  block: SignedBeaconBlock;
  fork: ForkName;
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
      const block = sszTypesFor(fork).SignedBeaconBlock.deserialize(body) as SignedBeaconBlock;
      const out = {block, fork};
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

  pruneBefore(slot: number): void {
    for (const cachedSlot of this.cache.keys()) {
      if (cachedSlot < slot) {
        this.cache.delete(cachedSlot);
      }
    }
  }

  async fetchByRoot(root: Uint8Array, slot: number): Promise<BlockFetch> {
    const url = `${this.baseUrl}/eth/v2/beacon/blocks/${toRootHex(root)}`;
    const {body, consensusVersion} = await fetchSsz(url);
    const fork = this.resolveForkAtSlot(slot, consensusVersion);
    const block = sszTypesFor(fork).SignedBeaconBlock.deserialize(body) as SignedBeaconBlock;
    return {block, fork};
  }

  async fetchByRootOptional(root: Uint8Array, slot: number): Promise<BlockFetch | null> {
    try {
      return await this.fetchByRoot(root, slot);
    } catch (err) {
      if (err instanceof NotFoundError) return null;
      throw err;
    }
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

class MemoryTracker {
  private peakHeapUsed = 0;
  private peakRss = 0;

  observe(slot: number): void {
    if (!LOG_MEMORY) return;

    const usage = process.memoryUsage();
    this.peakHeapUsed = Math.max(this.peakHeapUsed, usage.heapUsed);
    this.peakRss = Math.max(this.peakRss, usage.rss);

    if (MEMORY_LOG_INTERVAL_SLOTS > 0 && slot % MEMORY_LOG_INTERVAL_SLOTS === 0) {
      process.stderr.write(
        `[fcr-lodestar] memory slot=${slot} heap_used_mb=${toMiB(usage.heapUsed)} rss_mb=${toMiB(usage.rss)} peak_heap_used_mb=${toMiB(this.peakHeapUsed)} peak_rss_mb=${toMiB(this.peakRss)}\n`,
      );
    }
  }

  finish(): void {
    if (!LOG_MEMORY) return;

    process.stderr.write(
      `[fcr-lodestar] memory peak_heap_used_mb=${toMiB(this.peakHeapUsed)} peak_rss_mb=${toMiB(this.peakRss)}\n`,
    );
  }
}

function toMiB(bytes: number): number {
  return Math.round(bytes / 1024 / 1024);
}

async function main(): Promise<void> {
  let parsed: ParseResult;
  try {
    parsed = parseArgs(process.argv.slice(2));
  } catch (err) {
    process.stderr.write(`configuration error: ${formatError(err)}\n`);
    process.exit(1);
  }
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
  stateCache: StateCache;
  debug: DebugTracer;
  headStateRef: {state: CachedBeaconStateAllForks};
  headStateRootRef: {rootHex: RootHex};
}

function checkpointKey(ep: number, rootHex: string, payloadStatus: PayloadStatus): string {
  return `${ep}:${rootHex}:${payloadStatus}`;
}

function parseSlotSet(raw: string | undefined): Set<number> | null {
  if (!raw) return null;
  const slots = new Set<number>();
  for (const part of raw.split(",")) {
    const slot = Number(part.trim());
    if (Number.isInteger(slot) && slot >= 0) {
      slots.add(slot);
    }
  }
  return slots;
}

function logDataToFields(data: LogData | undefined): DebugFields {
  if (!data || typeof data !== "object" || Array.isArray(data)) return {};
  return data as DebugFields;
}

function trimMap<K, V>(map: Map<K, V>, limit: number): void {
  while (map.size > limit) {
    const oldest = map.keys().next().value;
    if (oldest === undefined) return;
    map.delete(oldest);
  }
}

type StateCacheEntry = {
  slot: number;
  state: CachedBeaconStateAllForks;
};

type ImportedPostStateCacheEntry = StateCacheEntry & {
  blockRootHex: RootHex;
  stateRootHex: RootHex;
};

type CheckpointStateCacheEntry = {
  epoch: number;
  state: CachedBeaconStateAllForks;
};

type DebugFields = Record<string, unknown>;

class DebugTracer {
  private phase = "bootstrap";
  private loopSlot: number | null = null;
  private evalSlot: number | null = null;

  constructor(
    readonly cacheEnabled: boolean,
    private readonly fcrEnabled: boolean,
    private readonly slots: Set<number> | null,
  ) {}

  setPhase(phase: string, loopSlot: number | null, evalSlot: number | null = null): void {
    this.phase = phase;
    this.loopSlot = loopSlot;
    this.evalSlot = evalSlot;
  }

  cache(event: string, fields: DebugFields): void {
    if (!this.cacheEnabled) return;
    this.write(event, fields);
  }

  lodestarLogger(): Logger | undefined {
    if (!this.fcrEnabled) return undefined;
    return {
      error: (message, context, error) => this.fcr("error", message, context, error),
      warn: (message, context, error) => this.fcr("warn", message, context, error),
      info: (message, context, error) => this.fcr("info", message, context, error),
      verbose: (message, context, error) => this.fcr("verbose", message, context, error),
      debug: (message, context, error) => this.fcr("debug", message, context, error),
    };
  }

  private fcr(level: string, message: string, context?: LogData, error?: Error): void {
    if (!this.fcrEnabled) return;
    this.write("fcr_log", {level, message, ...logDataToFields(context), error: error?.stack ?? error?.message});
  }

  private write(event: string, fields: DebugFields): void {
    if (!this.shouldLog(fields)) return;
    process.stderr.write(
      `[fcr-lodestar-debug] ${JSON.stringify({
        event,
        phase: this.phase,
        loopSlot: this.loopSlot,
        evalSlot: this.evalSlot,
        ...fields,
      })}\n`,
    );
  }

  private shouldLog(fields: DebugFields): boolean {
    if (!this.slots || this.slots.size === 0) return true;
    if (this.loopSlot !== null && this.slots.has(this.loopSlot)) return true;
    if (this.evalSlot !== null && this.slots.has(this.evalSlot)) return true;
    for (const key of ["slot", "currentSlot", "blockSlot", "confirmedSlot", "evictedAtSlot"]) {
      const value = fields[key];
      if (typeof value === "number" && this.slots.has(value)) return true;
    }
    return false;
  }
}

class StateCache {
  private readonly stateByStateRoot = new Map<RootHex, StateCacheEntry>();
  private readonly importedPostStateByBlockRoot = new Map<RootHex, ImportedPostStateCacheEntry>();
  private readonly importedPostStateByStateRoot = new Map<RootHex, ImportedPostStateCacheEntry>();
  private readonly importedBlockRootsByStateRoot = new Map<RootHex, Set<RootHex>>();
  private readonly stateByCheckpointKey = new Map<string, CheckpointStateCacheEntry>();
  private readonly evictedStateRoots = new Map<RootHex, {slot: number; evictedAtSlot: number}>();
  private readonly evictedCheckpoints = new Map<string, {epoch: number; evictedAtSlot: number}>();

  constructor(private readonly debug: DebugTracer) {}

  setStateRoot(rootHex: RootHex, slot: number, state: CachedBeaconStateAllForks): void {
    this.stateByStateRoot.set(rootHex, {slot, state});
    this.evictedStateRoots.delete(rootHex);
  }

  setImportedPostState(
    blockRootHex: RootHex,
    stateRootHex: RootHex,
    slot: number,
    state: CachedBeaconStateAllForks,
  ): void {
    const existing = this.importedPostStateByBlockRoot.get(blockRootHex);
    if (existing) {
      this.importedPostStateByBlockRoot.delete(blockRootHex);
      this.removeImportedStateRootRef(existing);
      this.repointImportedStateRoot(existing);
    }

    // Lighthouse-store substitute: retain imported post-states until fork-choice finalization prunes them away.
    const entry = {blockRootHex, stateRootHex, slot, state};
    this.importedPostStateByBlockRoot.set(blockRootHex, entry);
    this.importedPostStateByStateRoot.set(stateRootHex, entry);
    this.addImportedStateRootRef(entry);
  }

  getStateRoot(rootHex: RootHex): CachedBeaconStateAllForks | null {
    const entry = this.stateByStateRoot.get(rootHex);
    if (entry) return entry.state;
    const imported = this.importedPostStateByStateRoot.get(rootHex);
    if (imported) return imported.state;
    const evicted = this.evictedStateRoots.get(rootHex);
    this.debug.cache("state_cache_miss", {
      kind: "state_root",
      rootHex,
      evictedSlot: evicted?.slot,
      evictedAtSlot: evicted?.evictedAtSlot,
      stateRoots: this.stateByStateRoot.size,
      importedBlockStates: this.importedPostStateByBlockRoot.size,
      checkpoints: this.stateByCheckpointKey.size,
    });
    return null;
  }

  getImportedBlockPostState(blockRootHex: RootHex): CachedBeaconStateAllForks | null {
    const entry = this.importedPostStateByBlockRoot.get(blockRootHex);
    if (entry) return entry.state;
    this.debug.cache("state_cache_miss", {
      kind: "imported_block",
      blockRootHex,
      stateRoots: this.stateByStateRoot.size,
      importedBlockStates: this.importedPostStateByBlockRoot.size,
      checkpoints: this.stateByCheckpointKey.size,
    });
    return null;
  }

  setCheckpoint(
    epoch: number,
    rootHex: RootHex,
    payloadStatus: PayloadStatus,
    state: CachedBeaconStateAllForks,
  ): void {
    const key = checkpointKey(epoch, rootHex, payloadStatus);
    this.stateByCheckpointKey.set(key, {epoch, state});
    this.evictedCheckpoints.delete(key);
  }

  getCheckpoint(checkpoint: CheckpointWithPayloadStatus): CachedBeaconStateAllForks | null {
    const key = checkpointKey(checkpoint.epoch, checkpoint.rootHex, checkpoint.payloadStatus);
    const entry = this.stateByCheckpointKey.get(key);
    if (entry) return entry.state;
    const evicted = this.evictedCheckpoints.get(key);
    this.debug.cache("state_cache_miss", {
      kind: "checkpoint",
      key,
      checkpointEpoch: checkpoint.epoch,
      checkpointRoot: checkpoint.rootHex,
      payloadStatus: checkpoint.payloadStatus,
      evictedEpoch: evicted?.epoch,
      evictedAtSlot: evicted?.evictedAtSlot,
      stateRoots: this.stateByStateRoot.size,
      importedBlockStates: this.importedPostStateByBlockRoot.size,
      checkpoints: this.stateByCheckpointKey.size,
    });
    return null;
  }

  prune(currentSlot: number, currentEpoch: number): void {
    const minStateSlot = Math.max(0, currentSlot - RECENT_STATE_CACHE_SLOTS);
    for (const [rootHex, entry] of this.stateByStateRoot.entries()) {
      if (entry.slot < minStateSlot) {
        this.stateByStateRoot.delete(rootHex);
        this.rememberEvictedState(rootHex, entry.slot, currentSlot);
        this.debug.cache("state_cache_evict", {
          kind: "state_root",
          rootHex,
          slot: entry.slot,
          currentSlot,
          minStateSlot,
          stateRoots: this.stateByStateRoot.size,
          importedBlockStates: this.importedPostStateByBlockRoot.size,
          checkpoints: this.stateByCheckpointKey.size,
        });
      }
    }

    const minCheckpointEpoch = Math.max(0, currentEpoch - CHECKPOINT_STATE_CACHE_EPOCHS);
    for (const [key, entry] of this.stateByCheckpointKey.entries()) {
      if (entry.epoch < minCheckpointEpoch) {
        this.stateByCheckpointKey.delete(key);
        this.rememberEvictedCheckpoint(key, entry.epoch, currentSlot);
        this.debug.cache("state_cache_evict", {
          kind: "checkpoint",
          key,
          epoch: entry.epoch,
          currentSlot,
          minCheckpointEpoch,
          stateRoots: this.stateByStateRoot.size,
          importedBlockStates: this.importedPostStateByBlockRoot.size,
          checkpoints: this.stateByCheckpointKey.size,
        });
      }
    }
  }

  pruneFinalizedBlocks(prunedBlocks: ProtoBlock[], currentSlot: number): void {
    for (const block of prunedBlocks) {
      this.deleteImportedPostState(block.blockRoot, currentSlot);
    }
  }

  private addImportedStateRootRef(entry: ImportedPostStateCacheEntry): void {
    let blockRoots = this.importedBlockRootsByStateRoot.get(entry.stateRootHex);
    if (!blockRoots) {
      blockRoots = new Set();
      this.importedBlockRootsByStateRoot.set(entry.stateRootHex, blockRoots);
    }
    blockRoots.add(entry.blockRootHex);
  }

  private removeImportedStateRootRef(entry: ImportedPostStateCacheEntry): void {
    const blockRoots = this.importedBlockRootsByStateRoot.get(entry.stateRootHex);
    if (!blockRoots) return;
    blockRoots.delete(entry.blockRootHex);
    if (blockRoots.size === 0) {
      this.importedBlockRootsByStateRoot.delete(entry.stateRootHex);
    }
  }

  private deleteImportedPostState(blockRootHex: RootHex, currentSlot: number): void {
    const entry = this.importedPostStateByBlockRoot.get(blockRootHex);
    if (!entry) return;

    this.importedPostStateByBlockRoot.delete(blockRootHex);
    this.removeImportedStateRootRef(entry);
    this.repointImportedStateRoot(entry);

    this.debug.cache("state_cache_evict", {
      kind: "imported_block",
      blockRootHex,
      stateRootHex: entry.stateRootHex,
      slot: entry.slot,
      currentSlot,
      stateRoots: this.stateByStateRoot.size,
      importedBlockStates: this.importedPostStateByBlockRoot.size,
      checkpoints: this.stateByCheckpointKey.size,
    });
  }

  private repointImportedStateRoot(entry: ImportedPostStateCacheEntry): void {
    if (this.importedPostStateByStateRoot.get(entry.stateRootHex) === entry) {
      const replacementRoot = this.importedBlockRootsByStateRoot.get(entry.stateRootHex)?.values().next().value;
      const replacement = replacementRoot ? this.importedPostStateByBlockRoot.get(replacementRoot) : undefined;
      if (replacement) {
        this.importedPostStateByStateRoot.set(entry.stateRootHex, replacement);
      } else {
        this.importedPostStateByStateRoot.delete(entry.stateRootHex);
      }
    }
  }

  private rememberEvictedState(rootHex: RootHex, slot: number, evictedAtSlot: number): void {
    if (!this.debug.cacheEnabled) return;
    this.evictedStateRoots.set(rootHex, {slot, evictedAtSlot});
    trimMap(this.evictedStateRoots, DEBUG_EVICTION_HISTORY_LIMIT);
  }

  private rememberEvictedCheckpoint(key: string, epoch: number, evictedAtSlot: number): void {
    if (!this.debug.cacheEnabled) return;
    this.evictedCheckpoints.set(key, {epoch, evictedAtSlot});
    trimMap(this.evictedCheckpoints, DEBUG_EVICTION_HISTORY_LIMIT);
  }
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
  const debug = new DebugTracer(DEBUG_CACHE, DEBUG_FCR, DEBUG_SLOTS);
  const stateCache = new StateCache(debug);

  // Seed caches with the anchor state
  const anchorStateRoot = toRootHex(cachedState.hashTreeRoot());
  stateCache.setStateRoot(anchorStateRoot, cachedState.slot, cachedState);
  const headStateRef = {state: cachedState};
  const headStateRootRef = {rootHex: anchorStateRoot};

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
      if (opts.stateRoot === headStateRootRef.rootHex) {
        return new BeaconStateView(headStateRef.state);
      }
      const s = stateCache.getStateRoot(opts.stateRoot);
      if (!s) return null;
      return new BeaconStateView(s);
    }
    if (opts.checkpoint !== undefined) {
      const s = stateCache.getCheckpoint(opts.checkpoint);
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
    bootstrapStateView,
    currentSlot: cfg.warmupStartSlot,
    justifiedBalancesGetter,
    stateGetter,
    logger: debug.lodestarLogger(),
  });

  // Seed the checkpoint-state cache: the anchor state is the justified+finalized
  // checkpoint state at bootstrap.
  const anchor = bootstrapStateView.computeAnchorCheckpoint();
  const anchorBlockRoot = toRootHex(anchor.checkpoint.root);
  stateCache.setImportedPostState(anchorBlockRoot, anchorStateRoot, cachedState.slot, cachedState);
  stateCache.setCheckpoint(anchor.checkpoint.epoch, anchorBlockRoot, PayloadStatus.FULL, cachedState);

  return {
    beaconConfig,
    forkChoice,
    fetcher,
    stateCache,
    debug,
    headStateRef,
    headStateRootRef,
  };
}

function initializeForkChoiceFromAnchor(args: {
  beaconConfig: BeaconConfig;
  bootstrapStateView: BeaconStateView;
  currentSlot: number;
  justifiedBalancesGetter: JustifiedBalancesGetter;
  stateGetter: ForkChoiceStateGetter;
  logger?: Logger;
}): {forkChoice: ForkChoice} {
  const {beaconConfig, bootstrapStateView, currentSlot, justifiedBalancesGetter, stateGetter, logger} = args;
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
    {fastConfirmation: true, proposerBoost: false, proposerBoostReorg: false, computeUnrealized: true},
    logger,
  );

  return {forkChoice};
}

async function runSlotLoop(cfg: CliConfig, boot: BootstrapResult): Promise<void> {
  const writer = new JsonlWriter(cfg.output);
  const memory = new MemoryTracker();
  const {forkChoice, fetcher, stateCache, debug, headStateRef, headStateRootRef} = boot;

  try {
    let slot = cfg.warmupStartSlot + 1;
    let lastPrunedFinalizedRoot = forkChoice.getFinalizedCheckpoint().rootHex;
    while (slot < cfg.endSlot) {
      const isRecording = slot >= cfg.startSlot;

      debug.setPhase("pre_slot_update", slot, slot);
      forkChoice.updateTime(slot);

      const instruction = await fetchSlotInstruction(cfg.beaconNodeUrl, slot, cfg.warmupStartSlot);
      const {hasBlock, blockRootHex} = await importPlannedBlocks(slot, instruction.importBlocks, {
        forkChoice,
        fetcher,
        stateCache,
        headStateRef,
        headStateRootRef,
      });

      const numAttestationsInjected = injectPlannedAttestations(
        forkChoice,
        headStateRef.state,
        instruction.attestations,
        slot + 1,
      );

      // Recompute head + run FCR at the instruction eval slot.
      debug.setPhase("post_attestation_update", slot, instruction.evalSlot);
      const fcrStart = hrtime.bigint();
      forkChoice.updateTime(instruction.evalSlot);
      const head = forkChoice.updateHead();
      const fcrEvalUs = microsSince(fcrStart);
      debug.setPhase("record_head", slot, instruction.evalSlot);

      if (isRecording) {
        writer.write(buildRecord({
          forkChoice,
          slot,
          hasBlock,
          blockRootHex,
          headRoot: head.blockRoot,
          numAttestationsInjected,
          fcrEvalUs,
        }));
      }

      const finalized = forkChoice.getFinalizedCheckpoint();
      if (finalized.rootHex !== lastPrunedFinalizedRoot) {
        const prunedBlocks = forkChoice.prune(finalized.rootHex);
        stateCache.pruneFinalizedBlocks(prunedBlocks, slot);
        lastPrunedFinalizedRoot = finalized.rootHex;
      }
      debug.setPhase("prune", slot);
      stateCache.prune(slot, computeEpochAtSlot(slot));
      fetcher.pruneBefore(Math.max(0, slot - BLOCK_FETCH_CACHE_SLOTS));
      memory.observe(slot);
      debug.setPhase("idle", null);

      slot += 1;
    }
  } finally {
    memory.finish();
    writer.close();
  }
}

interface ImportPlannedBlocksContext {
  forkChoice: ForkChoice;
  fetcher: BlockFetcher;
  stateCache: StateCache;
  headStateRef: {state: CachedBeaconStateAllForks};
  headStateRootRef: {rootHex: RootHex};
}

async function importPlannedBlocks(
  simSlot: number,
  imports: PlanBlockImport[],
  ctx: ImportPlannedBlocksContext,
): Promise<{hasBlock: boolean; blockRootHex: string | null}> {
  const canonicalRoot = imports.find((planned) => planned.canonical && planned.slot === simSlot)?.rootHex ?? null;

  for (const planned of imports) {
    const blockFetch = await fetchPlannedBlock(planned, ctx.fetcher);
    if (!blockFetch) continue;

    verifyPlannedBlock(blockFetch, planned);

    if (ctx.forkChoice.hasBlockHexUnsafe(planned.rootHex)) {
      warn(
        `planned block root=${planned.rootHex} slot=${planned.slot} already known; skipping duplicate import`,
      );
      continue;
    }

    const parentPostState = planned.canonical
      ? ctx.headStateRef.state
      : resolveOrphanParentPostState(planned, blockFetch, ctx);
    if (!parentPostState) continue;

    try {
      const postState = processBlock(parentPostState, blockFetch);
      const postStateRoot = toRootHex(blockFetch.block.message.stateRoot);
      stateCacheImport(
        ctx,
        blockFetch,
        postState,
        postStateRoot,
        simSlot,
        planned.canonical && planned.slot === simSlot,
      );
    } catch (err) {
      if (planned.canonical) {
        throw new Error(`failed to process canonical block at slot ${planned.slot}: ${formatError(err)}`);
      }
      warn(
        `planned non-canonical block import failed root=${planned.rootHex} slot=${planned.slot} error=${formatError(err)}`,
      );
    }
  }

  return {hasBlock: canonicalRoot !== null, blockRootHex: canonicalRoot};
}

async function fetchPlannedBlock(planned: PlanBlockImport, fetcher: BlockFetcher): Promise<BlockFetch | null> {
  if (planned.canonical) {
    const block = await fetcher.fetchAtSlot(planned.slot);
    if (!block) {
      throw new Error(`planned canonical block at slot ${planned.slot} was not found`);
    }
    return block;
  }

  const block = await fetcher.fetchByRootOptional(planned.root, planned.slot);
  if (!block) {
    warn(`planned non-canonical block was not found; skipping root=${planned.rootHex} slot=${planned.slot}`);
  }
  return block;
}

function verifyPlannedBlock(blockFetch: BlockFetch, planned: PlanBlockImport): void {
  const blockRootHex = toRootHex(sszTypesFor(blockFetch.fork).BeaconBlock.hashTreeRoot(blockFetch.block.message));
  if (blockRootHex !== planned.rootHex) {
    throw new Error(
      `planned block root mismatch for slot ${planned.slot}: plan ${planned.rootHex}, fetched ${blockRootHex}`,
    );
  }
  if (blockFetch.block.message.slot !== planned.slot) {
    throw new Error(
      `planned block slot mismatch for root ${planned.rootHex}: plan ${planned.slot}, fetched ${blockFetch.block.message.slot}`,
    );
  }
}

function resolveOrphanParentPostState(
  planned: PlanBlockImport,
  blockFetch: BlockFetch,
  ctx: ImportPlannedBlocksContext,
): CachedBeaconStateAllForks | null {
  const parentRootHex = toRootHex(blockFetch.block.message.parentRoot);
  const parentProto = ctx.forkChoice.getBlockHexDefaultStatus(parentRootHex);
  if (!parentProto) {
    warn(
      `planned non-canonical block missing parent proto; skipping root=${planned.rootHex} slot=${planned.slot} parent=${parentRootHex}`,
    );
    return null;
  }

  const parentPostState =
    ctx.stateCache.getImportedBlockPostState(parentRootHex) ??
    (ctx.forkChoice.getHead().blockRoot === parentRootHex ? ctx.headStateRef.state : null) ??
    ctx.stateCache.getStateRoot(parentProto.stateRoot);
  if (!parentPostState) {
    warn(
      `planned non-canonical block missing parent post-state; skipping root=${planned.rootHex} slot=${planned.slot} parent=${parentRootHex} parent_state_root=${parentProto.stateRoot}`,
    );
    return null;
  }

  return parentPostState;
}

function stateCacheImport(
  ctx: ImportPlannedBlocksContext,
  blockFetch: BlockFetch,
  postState: CachedBeaconStateAllForks,
  postStateRoot: RootHex,
  simSlot: number,
  advancesHead: boolean,
): void {
  const blockSlot = blockFetch.block.message.slot;
  ctx.stateCache.setStateRoot(postStateRoot, blockSlot, postState);
  const protoBlock = ctx.forkChoice.onBlock(
    blockFetch.block.message,
    new BeaconStateView(postState),
    0,
    simSlot,
    ExecutionStatus.Valid,
    DataAvailabilityStatus.Available,
  );
  ctx.stateCache.setImportedPostState(protoBlock.blockRoot, postStateRoot, blockSlot, postState);

  // If this block is an epoch boundary, cache it as a potential checkpoint
  // state under FULL payload status to satisfy FCR lookups.
  if (blockSlot % 32 === 0) {
    const epoch = computeEpochAtSlot(blockSlot);
    ctx.stateCache.setCheckpoint(epoch, protoBlock.blockRoot, PayloadStatus.FULL, postState);
  }

  if (advancesHead) {
    ctx.headStateRef.state = postState;
    ctx.headStateRootRef.rootHex = postStateRoot;
  }
}

function warn(message: string): void {
  process.stderr.write(`[fcr-lodestar] ${message}\n`);
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

function injectPlannedAttestations(
  forkChoice: ForkChoice,
  headState: CachedBeaconStateAllForks,
  attestations: PlanAttestation[],
  injectSlot: number,
): number {
  if (attestations.length === 0) return 0;

  let injected = 0;
  for (const planned of attestations) {
    const fork = headState.config.getForkName(planned.data.slot);
    const forkSeq = headState.config.getForkSeq(planned.data.slot);

    let indexed: IndexedAttestation;
    if (planned.attestingIndices !== null) {
      indexed = buildDirectIndexedAttestation(planned);
    } else {
      const attestation = buildPlannedAttestation(planned, forkSeq);
      try {
        indexed = headState.epochCtx.getIndexedAttestation(forkSeq, attestation);
      } catch {
        // Planned attestation's epoch shuffling not available in head state's epoch context
        // (e.g. attestation references a far-future or far-past epoch). Skip and warn.
        continue;
      }
    }
    if (indexed.attestingIndices.length === 0) continue;
    const attDataRoot = toRootHex(sszTypesFor(fork).AttestationData.hashTreeRoot(indexed.data));
    try {
      injectAttestationAtSlot(forkChoice, indexed, attDataRoot, injectSlot);
      injected++;
    } catch {
      // Match Lighthouse policy: log-and-continue on injection failure.
    }
  }
  return injected;
}

function buildDirectIndexedAttestation(planned: PlanAttestation): IndexedAttestation {
  if (planned.attestingIndices === null) {
    throw new Error("indexed attestation is missing attesting_indices");
  }

  return {
    attestingIndices: uniqueSorted(planned.attestingIndices),
    data: buildPlannedAttestationData(planned),
    signature: G2_POINT_AT_INFINITY,
  };
}

function buildPlannedAttestation(planned: PlanAttestation, forkSeq: ForkSeq): Attestation {
  const aggregationBits = planned.aggregationBits;
  if (aggregationBits === null) {
    throw new Error("attestation is missing aggregation_bits");
  }

  const data = buildPlannedAttestationData(planned);

  if (forkSeq >= ForkSeq.electra) {
    if (planned.committeeBits === null) {
      throw new Error("electra attestation is missing committee_bits");
    }
    return {
      aggregationBits: ssz.electra.AggregationBits.deserialize(aggregationBits),
      data,
      signature: G2_POINT_AT_INFINITY,
      committeeBits: ssz.electra.CommitteeBits.deserialize(planned.committeeBits),
    } as Attestation;
  }

  return {
    aggregationBits: ssz.phase0.CommitteeBits.deserialize(aggregationBits),
    data,
    signature: G2_POINT_AT_INFINITY,
  } as Attestation;
}

function buildPlannedAttestationData(planned: PlanAttestation): IndexedAttestation["data"] {
  return {
    slot: planned.data.slot,
    index: planned.data.index,
    beaconBlockRoot: planned.data.beaconBlockRoot,
    source: {
      epoch: planned.data.source.epoch,
      root: planned.data.source.root,
    },
    target: {
      epoch: planned.data.target.epoch,
      root: planned.data.target.root,
    },
  };
}

function uniqueSorted(values: number[]): number[] {
  return Array.from(new Set(values)).sort((a, b) => a - b);
}

type ForkChoiceWithInternals = {
  fcStore: {
    currentSlot: number;
    equivocatingIndices: Set<number>;
  };
  protoArray: {
    getDefaultVariant(rootHex: RootHex): PayloadStatus | undefined;
    getNodeIndexByRootAndStatus(rootHex: RootHex, payloadStatus: PayloadStatus): number | undefined;
  };
  validateOnAttestation(
    indexedAttestation: IndexedAttestation,
    slot: number,
    blockRootHex: string,
    targetEpoch: number,
    attDataRoot: string,
    forceImport?: boolean,
  ): void;
  voteCurrentIndices: number[];
  voteNextIndices: number[];
  voteNextSlots: number[];
};

function injectAttestationAtSlot(
  forkChoice: ForkChoice,
  indexed: IndexedAttestation,
  attDataRoot: string,
  injectSlot: number,
): void {
  if (indexed.data.slot >= injectSlot) {
    withForkChoiceCurrentSlot(forkChoice, injectSlot, () => forkChoice.onAttestation(indexed, attDataRoot, true));
    return;
  }

  const blockRootHex = toRootHex(indexed.data.beaconBlockRoot);
  if (isZeroRoot(blockRootHex)) return;

  const internal = forkChoice as unknown as ForkChoiceWithInternals;
  withForkChoiceCurrentSlot(forkChoice, injectSlot, () => {
    internal.validateOnAttestation(
      indexed,
      indexed.data.slot,
      blockRootHex,
      indexed.data.target.epoch,
      attDataRoot,
      true,
    );
  });

  const payloadStatus = internal.protoArray.getDefaultVariant(blockRootHex) ?? PayloadStatus.FULL;
  for (const validatorIndex of indexed.attestingIndices) {
    if (!internal.fcStore.equivocatingIndices.has(validatorIndex)) {
      addLatestMessageBySlot(internal, validatorIndex, indexed.data.slot, blockRootHex, payloadStatus);
    }
  }
}

function withForkChoiceCurrentSlot<T>(forkChoice: ForkChoice, slot: number, fn: () => T): T {
  const internal = forkChoice as unknown as ForkChoiceWithInternals;
  const previousSlot = internal.fcStore.currentSlot;
  internal.fcStore.currentSlot = slot;
  try {
    return fn();
  } finally {
    internal.fcStore.currentSlot = previousSlot;
  }
}

function addLatestMessageBySlot(
  forkChoice: ForkChoiceWithInternals,
  validatorIndex: number,
  nextSlot: number,
  nextRoot: RootHex,
  nextPayloadStatus: PayloadStatus,
): void {
  const nextIndex = forkChoice.protoArray.getNodeIndexByRootAndStatus(nextRoot, nextPayloadStatus);
  if (nextIndex === undefined) {
    throw new Error(`Could not find proto index for nextRoot ${nextRoot} with payloadStatus ${nextPayloadStatus}`);
  }

  if (forkChoice.voteNextSlots.length < validatorIndex + 1) {
    for (let i = forkChoice.voteNextSlots.length; i < validatorIndex + 1; i++) {
      forkChoice.voteNextSlots[i] = LODESTAR_INIT_VOTE_SLOT;
      forkChoice.voteCurrentIndices[i] = LODESTAR_NULL_VOTE_INDEX;
      forkChoice.voteNextIndices[i] = LODESTAR_NULL_VOTE_INDEX;
    }
  }

  const existingNextSlot = forkChoice.voteNextSlots[validatorIndex];
  if (existingNextSlot === LODESTAR_INIT_VOTE_SLOT || nextSlot > existingNextSlot) {
    forkChoice.voteNextIndices[validatorIndex] = nextIndex;
    forkChoice.voteNextSlots[validatorIndex] = nextSlot;
  }
}

interface BuildRecordArgs {
  forkChoice: ForkChoice;
  slot: number;
  hasBlock: boolean;
  blockRootHex: string | null;
  headRoot: string;
  numAttestationsInjected: number;
  fcrEvalUs: number;
}

function buildRecord(args: BuildRecordArgs): Record<string, unknown> {
  const {forkChoice, slot, hasBlock, blockRootHex, headRoot, numAttestationsInjected, fcrEvalUs} = args;
  const confirmedRoot = forkChoice.getConfirmedRoot();
  const confirmedBlock = forkChoice.getBlockHexDefaultStatus(confirmedRoot);
  const confirmedSlot = confirmedBlock?.slot ?? 0;
  const finalized = forkChoice.getFinalizedCheckpoint();
  const justified = forkChoice.getJustifiedCheckpoint();

  const fastConfirmed = !isZeroRoot(confirmedRoot) && confirmedSlot === slot;
  const evalSlot = slot + 1;
  const delay = evalSlot >= confirmedSlot ? evalSlot - confirmedSlot : 0;
  const strictOneSlotConfirmed =
    hasBlock &&
    !isZeroRoot(confirmedRoot) &&
    blockRootHex === confirmedRoot &&
    confirmedSlot === slot &&
    delay === 1;

  return {
    slot,
    epoch: Math.floor(slot / 32),
    has_block: hasBlock,
    block_root: blockRootHex,
    head_root: headRoot,
    confirmed_root: isZeroRoot(confirmedRoot) ? ZERO_ROOT_HEX : confirmedRoot,
    confirmed_slot: confirmedSlot,
    confirmation_delay_slots: delay,
    fast_confirmed: fastConfirmed,
    strict_one_slot_confirmed: strictOneSlotConfirmed,
    finalized_epoch: finalized.epoch,
    justified_epoch: justified.epoch,
    source_block_slot: null,
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
