# fcr-nimbus: Nimbus FCR replay engine for fcr-simulator.
#
# Implements the engine contract documented in fcr-simulator/docs/ENGINE_CONTRACT.md.
# Drives nimbus-eth2's headless ChainDAGRef + AttestationPool fork choice across a
# warmup/recording slot range, emitting JSONL records that the Go orchestrator merges
# into the final per-run CSV.

import
  std/[json, monotimes, options, os, parseopt, strformat, strutils, tables, times],
  chronos,
  chronos/apps/http/httpclient,
  results,
  stew/[byteutils, io2],
  taskpools

import beacon_chain/beacon_chain_db
import beacon_chain/consensus_object_pools/attestation_pool
import beacon_chain/consensus_object_pools/blockchain_dag
import beacon_chain/consensus_object_pools/block_clearance
import beacon_chain/consensus_object_pools/block_quarantine
import beacon_chain/consensus_object_pools/spec_cache
import beacon_chain/fork_choice/fork_choice
import beacon_chain/fork_choice/fork_choice_types
import beacon_chain/gossip_processing/batch_validation
import beacon_chain/spec/beaconstate
import beacon_chain/spec/forks
import beacon_chain/spec/helpers
import beacon_chain/spec/presets
import beacon_chain/spec/state_transition
import beacon_chain/spec/state_transition_epoch
import beacon_chain/spec/datatypes/base
import beacon_chain/spec/datatypes/phase0
import beacon_chain/spec/datatypes/altair
import beacon_chain/spec/datatypes/bellatrix
import beacon_chain/spec/datatypes/capella
import beacon_chain/spec/datatypes/deneb
import beacon_chain/spec/datatypes/electra
import beacon_chain/validators/validator_monitor
import beacon_chain/networking/network_metadata

const
  EngineName = "nimbus"
  EngineVersion = block:
    const VersionMod = "../nimbus-eth2/beacon_chain/version"
    when fileExists(VersionMod & ".nim"):
      "v26.5.0"
    else:
      "v26.5.0"
  EngineCommit = "6fb05f36804d53c2e8e014cfeeea8ad7996a5efe"
  FcrSpecCommit = ""
  FlushInterval = 100

type
  EngineConfig = object
    beaconNodeUrl: string
    startSlot: Slot
    endSlot: Slot
    warmupStartSlot: Slot
    network: string
    byzantineThreshold: uint64
    attestationSourceMode: string
    lookaheadCap: uint64
    output: string
    manifestJson: bool

  PlanCheckpoint = object
    epoch: Epoch
    root: Eth2Digest

  PlanAttestationData = object
    slot: Slot
    index: uint64
    beaconBlockRoot: Eth2Digest
    source: PlanCheckpoint
    target: PlanCheckpoint

  PlanAttestation = object
    aggregationBits: seq[byte]
    committeeBits: Option[seq[byte]]
    data: PlanAttestationData

  PlanBlockImport = object
    slot: Slot
    root: Eth2Digest
    canonical: bool

  SlotInstruction = object
    simSlot: Slot
    evalSlot: Slot
    importBlocks: seq[PlanBlockImport]
    attestations: seq[PlanAttestation]

  EngineError = object of CatchableError

  ExitKind = enum
    ekOk = 0
    ekConfig = 1
    ekBootstrap = 2
    ekRunFailure = 3

# --------------------------------------------------------------------------------
# CLI parsing

proc parseEngineConfig(): tuple[cfg: EngineConfig, exit: Option[ExitKind]] =
  var cfg = EngineConfig(
    network: "",
    byzantineThreshold: 25,
    attestationSourceMode: "",
    lookaheadCap: 0)
  let argv = commandLineParams()
  var i = 0
  template needArg(flag: string): string =
    if i + 1 >= argv.len:
      stderr.writeLine "missing argument for " & flag
      return (cfg, some(ekConfig))
    inc i
    argv[i]
  while i < argv.len:
    let arg = argv[i]
    let (key, valOpt) =
      if arg.startsWith("--"):
        let stripped = arg[2 .. ^1]
        let eq = stripped.find('=')
        if eq >= 0:
          (stripped[0 ..< eq], some(stripped[eq + 1 .. ^1]))
        else:
          (stripped, none(string))
      else:
        stderr.writeLine "unexpected positional argument: " & arg
        return (cfg, some(ekConfig))
    template getVal(flag: string): string =
      if valOpt.isSome: valOpt.get else: needArg(flag)
    case key
    of "beacon-node-url": cfg.beaconNodeUrl = getVal("--beacon-node-url")
    of "start-slot":
      let v = getVal("--start-slot")
      try: cfg.startSlot = Slot(parseBiggestUInt(v))
      except ValueError:
        stderr.writeLine "invalid --start-slot: " & v
        return (cfg, some(ekConfig))
    of "end-slot":
      let v = getVal("--end-slot")
      try: cfg.endSlot = Slot(parseBiggestUInt(v))
      except ValueError:
        stderr.writeLine "invalid --end-slot: " & v
        return (cfg, some(ekConfig))
    of "warmup-start-slot":
      let v = getVal("--warmup-start-slot")
      try: cfg.warmupStartSlot = Slot(parseBiggestUInt(v))
      except ValueError:
        stderr.writeLine "invalid --warmup-start-slot: " & v
        return (cfg, some(ekConfig))
    of "network": cfg.network = getVal("--network")
    of "byzantine-threshold":
      let v = getVal("--byzantine-threshold")
      try: cfg.byzantineThreshold = parseBiggestUInt(v).uint64
      except ValueError:
        stderr.writeLine "invalid --byzantine-threshold: " & v
        return (cfg, some(ekConfig))
    of "attestation-source-mode":
      cfg.attestationSourceMode = getVal("--attestation-source-mode")
    of "lookahead-cap":
      let v = getVal("--lookahead-cap")
      try: cfg.lookaheadCap = parseBiggestUInt(v).uint64
      except ValueError:
        stderr.writeLine "invalid --lookahead-cap: " & v
        return (cfg, some(ekConfig))
    of "output": cfg.output = getVal("--output")
    of "manifest-json": cfg.manifestJson = true
    else:
      stderr.writeLine "unknown flag --" & key
      return (cfg, some(ekConfig))
    inc i

  if cfg.manifestJson:
    return (cfg, none(ExitKind))

  if cfg.beaconNodeUrl.len == 0:
    stderr.writeLine "missing --beacon-node-url"
    return (cfg, some(ekConfig))
  if cfg.network != "mainnet":
    stderr.writeLine "--network must be 'mainnet' (V1)"
    return (cfg, some(ekConfig))
  if cfg.warmupStartSlot > cfg.startSlot:
    stderr.writeLine "--warmup-start-slot must be <= --start-slot"
    return (cfg, some(ekConfig))
  if cfg.startSlot >= cfg.endSlot:
    stderr.writeLine "--start-slot must be < --end-slot"
    return (cfg, some(ekConfig))
  if cfg.output.len == 0:
    stderr.writeLine "missing --output"
    return (cfg, some(ekConfig))

  (cfg, none(ExitKind))

proc printManifest() =
  let manifest = %*{
    "engine_name": EngineName,
    "engine_version": EngineVersion,
    "engine_commit": EngineCommit,
    "build_flags": ["fake_crypto", "no_el_notify", "skip_blob_da"],
    "fcr_spec_commit": FcrSpecCommit,
  }
  stdout.write($manifest)
  stdout.write("\n")

# --------------------------------------------------------------------------------
# HTTP fetches

proc trimBaseUrl(url: string): string =
  if url.endsWith("/"): url[0 ..< ^1] else: url

proc parsePlanRoot(value, ctx: string): Eth2Digest =
  if not value.startsWith("0x"):
    raise newException(EngineError, ctx & ": root must have 0x prefix")
  if value.len != 66:
    raise newException(EngineError,
      &"{ctx}: expected 32-byte root hex string, got {value.len - 2} hex chars")
  try:
    Eth2Digest(data: hexToByteArrayStrict[32](value))
  except ValueError as e:
    raise newException(EngineError, ctx & ": invalid root hex: " & e.msg)

proc parsePlanHexBytes(value, ctx: string): seq[byte] =
  try:
    hexToSeqByte(value)
  except ValueError as e:
    raise newException(EngineError, ctx & ": invalid hex: " & e.msg)

proc requireObject(node: JsonNode, ctx: string) =
  if node.isNil or node.kind != JObject:
    raise newException(EngineError, ctx & " must be an object")

proc requireField(node: JsonNode, field, ctx: string): JsonNode =
  requireObject(node, ctx)
  if not node.hasKey(field):
    raise newException(EngineError, ctx & " is missing " & field)
  node[field]

proc requireArrayField(node: JsonNode, field, ctx: string): JsonNode =
  let child = requireField(node, field, ctx)
  if child.kind != JArray:
    raise newException(EngineError, ctx & "." & field & " must be an array")
  child

proc requireStringField(node: JsonNode, field, ctx: string): string =
  let child = requireField(node, field, ctx)
  if child.kind != JString:
    raise newException(EngineError, ctx & "." & field & " must be a string")
  child.getStr()

proc requireUint64Field(node: JsonNode, field, ctx: string): uint64 =
  let child = requireField(node, field, ctx)
  if child.kind != JInt:
    raise newException(EngineError, ctx & "." & field & " must be an integer")
  let value = child.getBiggestInt()
  if value < 0:
    raise newException(EngineError, ctx & "." & field & " must be non-negative")
  value.uint64

proc requireBoolField(node: JsonNode, field, ctx: string): bool =
  let child = requireField(node, field, ctx)
  if child.kind != JBool:
    raise newException(EngineError, ctx & "." & field & " must be a bool")
  child.getBool()

proc parseCheckpoint(node: JsonNode, ctx: string): PlanCheckpoint =
  requireObject(node, ctx)
  PlanCheckpoint(
    epoch: Epoch(requireUint64Field(node, "epoch", ctx)),
    root: parsePlanRoot(requireStringField(node, "root", ctx), ctx & ".root"))

proc parseAttestationData(node: JsonNode, ctx: string): PlanAttestationData =
  requireObject(node, ctx)
  PlanAttestationData(
    slot: Slot(requireUint64Field(node, "slot", ctx)),
    index: requireUint64Field(node, "index", ctx),
    beaconBlockRoot: parsePlanRoot(
      requireStringField(node, "beacon_block_root", ctx),
      ctx & ".beacon_block_root"),
    source: parseCheckpoint(requireField(node, "source", ctx), ctx & ".source"),
    target: parseCheckpoint(requireField(node, "target", ctx), ctx & ".target"))

proc parsePlanAttestation(node: JsonNode, ctx: string): PlanAttestation =
  requireObject(node, ctx)
  var committeeBits = none(seq[byte])
  if node.hasKey("committee_bits"):
    let committeeNode = node["committee_bits"]
    if committeeNode.kind != JNull:
      if committeeNode.kind != JString:
        raise newException(EngineError,
          ctx & ".committee_bits must be a string or null")
      committeeBits = some(parsePlanHexBytes(
        committeeNode.getStr(), ctx & ".committee_bits"))
  PlanAttestation(
    aggregationBits: parsePlanHexBytes(
      requireStringField(node, "aggregation_bits", ctx),
      ctx & ".aggregation_bits"),
    committeeBits: committeeBits,
    data: parseAttestationData(requireField(node, "data", ctx), ctx & ".data"))

proc parsePlanBlockImport(node: JsonNode, ctx: string): PlanBlockImport =
  requireObject(node, ctx)
  PlanBlockImport(
    slot: Slot(requireUint64Field(node, "slot", ctx)),
    root: parsePlanRoot(requireStringField(node, "root", ctx), ctx & ".root"),
    canonical: requireBoolField(node, "canonical", ctx))

proc parseSlotInstruction(body: seq[byte], simSlot: Slot): SlotInstruction =
  let parsed = try:
    parseJson(string.fromBytes(body))
  except CatchableError as e:
    raise newException(EngineError,
      "failed to decode slot instruction JSON: " & e.msg)

  let version = requireUint64Field(parsed, "version", "slot instruction response")
  if version != 3:
    raise newException(EngineError,
      &"unsupported slot instruction version {version}; expected 3")

  let slotNode = requireField(parsed, "slot", "slot instruction response")
  let gotSimSlot = requireUint64Field(slotNode, "sim_slot", "slot instruction")
  if gotSimSlot != simSlot.uint64:
    raise newException(EngineError,
      "slot instruction response sim_slot mismatch: requested " &
      $simSlot.uint64 & ", got " & $gotSimSlot)

  var imports: seq[PlanBlockImport]
  let importsNode =
    requireArrayField(slotNode, "import_blocks", "slot instruction")
  var importIdx = 0
  for importNode in importsNode:
    imports.add(parsePlanBlockImport(
      importNode, &"slot instruction.import_blocks[{importIdx}]"))
    inc importIdx

  var attestations: seq[PlanAttestation]
  let attestationsNode =
    requireArrayField(slotNode, "attestations", "slot instruction")
  var attestationIdx = 0
  for attestationNode in attestationsNode:
    attestations.add(parsePlanAttestation(
      attestationNode, &"slot instruction.attestations[{attestationIdx}]"))
    inc attestationIdx

  SlotInstruction(
    simSlot: Slot(gotSimSlot),
    evalSlot: Slot(requireUint64Field(slotNode, "eval_slot", "slot instruction")),
    importBlocks: imports,
    attestations: attestations)

proc httpGet(session: HttpSessionRef, url: string, acceptSsz: bool):
    Future[tuple[status: int, body: seq[byte]]]
    {.async: (raises: [CatchableError]).} =
  let acceptVal = if acceptSsz: "application/octet-stream" else: "application/json"
  let headers = @[("Accept", acceptVal)]
  let reqRes = HttpClientRequestRef.new(
    session, url, MethodGet, headers = headers)
  if reqRes.isErr:
    raise newException(EngineError, "bad request for " & url)
  var req = reqRes.get
  var resp: HttpClientResponseRef = nil
  try:
    resp = await req.send()
    let body = await resp.getBodyBytes()
    result = (status: resp.status.int, body: body)
  finally:
    if resp != nil: await resp.closeWait()
    await req.closeWait()

proc fetchSszBlockAtSlot(session: HttpSessionRef, base: string, slot: Slot,
    cfg: RuntimeConfig):
    Future[Option[ForkedSignedBeaconBlock]]
    {.async: (raises: [CatchableError]).} =
  let url = base & "/eth/v2/beacon/blocks/" & $slot.uint64
  let (status, body) = await httpGet(session, url, acceptSsz = true)
  if status == 404:
    return none(ForkedSignedBeaconBlock)
  if status != 200:
    raise newException(EngineError, &"HTTP {status} from {url}")
  let blck = readSszForkedSignedBeaconBlock(cfg, body)
  some(blck)

proc fetchSszBlockByRootOptional(session: HttpSessionRef, base: string,
    root: Eth2Digest, cfg: RuntimeConfig):
    Future[Option[ForkedSignedBeaconBlock]]
    {.async: (raises: [CatchableError]).} =
  let url = base & "/eth/v2/beacon/blocks/0x" & root.data.toHex()
  let (status, body) = await httpGet(session, url, acceptSsz = true)
  if status == 404:
    return none(ForkedSignedBeaconBlock)
  if status != 200:
    raise newException(EngineError, &"HTTP {status} from {url}")
  some(readSszForkedSignedBeaconBlock(cfg, body))

proc fetchSszBlockByRoot(session: HttpSessionRef, base: string,
    root: Eth2Digest, cfg: RuntimeConfig):
    Future[ForkedSignedBeaconBlock] {.async: (raises: [CatchableError]).} =
  let blck = await fetchSszBlockByRootOptional(session, base, root, cfg)
  if blck.isNone:
    let url = base & "/eth/v2/beacon/blocks/0x" & root.data.toHex()
    raise newException(EngineError, &"HTTP 404 from {url}")
  blck.get

proc fetchSlotInstruction(session: HttpSessionRef, base: string,
    simSlot, warmupStartSlot: Slot):
    Future[SlotInstruction] {.async: (raises: [CatchableError]).} =
  let url = base & "/fcr-sim/v1/slot/" & $simSlot.uint64 &
    "?warmup_start_slot=" & $warmupStartSlot.uint64
  let (status, body) = await httpGet(session, url, acceptSsz = false)
  if status != 200:
    raise newException(EngineError, &"HTTP {status} from {url}")
  parseSlotInstruction(body, simSlot)

proc fetchCheckpointState(session: HttpSessionRef, base: string, slot: Slot,
    cfg: RuntimeConfig):
    Future[ForkedHashedBeaconState] {.async: (raises: [CatchableError]).} =
  let url = base & "/eth/v2/debug/beacon/states/" & $slot.uint64
  let (status, body) = await httpGet(session, url, acceptSsz = true)
  if status != 200:
    raise newException(EngineError, &"HTTP {status} from {url}")
  readSszForkedHashedBeaconState(cfg, body)

proc fetchGenesisState(session: HttpSessionRef, base: string,
    cfg: RuntimeConfig):
    Future[ForkedHashedBeaconState] {.async: (raises: [CatchableError]).} =
  let url = base & "/eth/v2/debug/beacon/states/genesis"
  let (status, body) = await httpGet(session, url, acceptSsz = true)
  if status != 200:
    raise newException(EngineError, &"HTTP {status} from {url}")
  readSszForkedHashedBeaconState(cfg, body)

# --------------------------------------------------------------------------------
# Engine core

type
  Engine = ref object
    cfg: EngineConfig
    spec: RuntimeConfig
    session: HttpSessionRef
    base: string
    db: BeaconChainDB
    rng: ref HmacDrbgContext
    taskpool: Taskpool
    dag: ChainDAGRef
    quarantine: ref Quarantine
    attPool: ref AttestationPool
    batchVerifier: ref BatchVerifier
    validatorMonitor: ref ValidatorMonitor
    blockCache: Table[uint64, Option[ForkedSignedBeaconBlock]]
    outFile: File
    recordsWritten: uint64

proc mainnetSpec(byzantineThreshold: uint64): RuntimeConfig =
  result = getMetadataForNetwork("mainnet").cfg
  result.CONFIRMATION_BYZANTINE_THRESHOLD = byzantineThreshold

proc init(T: type Engine, cfg: EngineConfig): Future[T]
    {.async: (raises: [CatchableError]).} =
  var eng = Engine()
  eng.cfg = cfg
  eng.spec = mainnetSpec(cfg.byzantineThreshold)
  eng.session = HttpSessionRef.new()
  eng.base = trimBaseUrl(cfg.beaconNodeUrl)

  stderr.writeLine "[fcr-nimbus] fetching checkpoint state at slot " &
    $cfg.warmupStartSlot.uint64
  let checkpointState =
    await fetchCheckpointState(eng.session, eng.base, cfg.warmupStartSlot, eng.spec)
  let genesisState =
    await fetchGenesisState(eng.session, eng.base, eng.spec)

  let memDb = BeaconChainDB.new("", eng.spec, inMemory = true)
  ChainDAGRef.preInit(memDb, checkpointState)

  let validatorMonitor = newClone(ValidatorMonitor.init(eng.spec))
  eng.validatorMonitor = validatorMonitor
  eng.db = memDb

  # skipBlsValidation is set on the dag via updateFlags. ChainDAGRef.init only
  # accepts {strictVerification}; we add skipBlsValidation post-init.
  let dag = ChainDAGRef.init(eng.spec, memDb, validatorMonitor, {})
  dag.updateFlags = {skipBlsValidation, skipStateRootValidation}
  eng.dag = dag

  let quarantine = newClone(Quarantine.init(eng.spec))
  eng.quarantine = quarantine

  let attPool = newClone(AttestationPool.init(
    dag, quarantine, wallTime = cfg.warmupStartSlot.start_beacon_time(
      dag.timeParams)))
  eng.attPool = attPool

  eng.rng = HmacDrbgContext.new()
  eng.taskpool = Taskpool.new()
  eng.batchVerifier = newClone(
    BatchVerifier.new(eng.rng, eng.taskpool))

  # Open output file (overwrite mode).
  eng.outFile = open(cfg.output, fmWrite)

  return eng

proc getBlockAtSlot(self: Engine, slot: Slot):
    Future[Option[ForkedSignedBeaconBlock]]
    {.async: (raises: [CatchableError]).} =
  let key = slot.uint64
  if key in self.blockCache:
    return self.blockCache[key]
  let blck = await fetchSszBlockAtSlot(
    self.session, self.base, slot, self.spec)
  self.blockCache[key] = blck
  blck

proc makeOnBlockAdded(self: Engine, wallTime: BeaconTime, consensusFork: static ConsensusFork):
    OnBlockAdded[consensusFork] =
  proc(blckRef: BlockRef, blck: consensusFork.TrustedSignedBeaconBlock,
       state: consensusFork.BeaconState, epochRef: EpochRef,
       unrealized: FinalityCheckpoints) {.gcsafe, raises: [].} =
    self.attPool[].addForkChoice(
      epochRef, blckRef, unrealized, blck.message, wallTime)

func forkedBlockRoot(forked: ForkedSignedBeaconBlock): Eth2Digest =
  withBlck(forked): forkyBlck.root

func forkedBlockSlot(forked: ForkedSignedBeaconBlock): Slot =
  withBlck(forked): forkyBlck.message.slot

proc processBlock(self: Engine, forked: ForkedSignedBeaconBlock):
    Result[bool, string] =
  var imported = false
  var duplicate = false
  var errMsg = ""
  withBlck(forked):
    let parentRes = checkHeadBlock(self.dag, forkyBlck)
    if parentRes.isErr:
      let parentErr = results.error(parentRes)
      if parentErr == VerifierError.Duplicate:
        duplicate = true
      else:
        errMsg = "checkHeadBlock failed: " & $parentErr
    else:
      let parent = parentRes.value
      let wallTime = forkyBlck.message.slot.start_beacon_time(self.dag.cfg.timeParams)
      let cb = makeOnBlockAdded(self, wallTime, consensusFork)
      let addRes = addHeadBlockWithParent(
        self.dag, self.batchVerifier[], forkyBlck, parent,
        OptimisticStatus.valid, cb)
      if addRes.isErr:
        let addErr = results.error(addRes)
        if addErr == VerifierError.Duplicate:
          duplicate = true
        else:
          errMsg = "addHeadBlock failed: " & $addErr
      else:
        imported = true
  if errMsg.len > 0:
    return err(errMsg)
  if duplicate:
    return ok(false)
  if not imported:
    return err("processBlock: no import result")
  ok(true)

proc importPlannedBlocks(self: Engine, simSlot: Slot,
    imports: seq[PlanBlockImport]):
    Future[tuple[hasBlock: bool, blockRoot: Option[Eth2Digest]]]
    {.async: (raises: [CatchableError]).} =
  var blockRoot = none(Eth2Digest)
  for plan in imports:
    if plan.canonical and plan.slot == simSlot and blockRoot.isNone:
      blockRoot = some(plan.root)

  for plan in imports:
    var blockOpt: Option[ForkedSignedBeaconBlock]
    if plan.canonical:
      blockOpt = await self.getBlockAtSlot(plan.slot)
      if blockOpt.isNone:
        raise newException(EngineError,
          "planned canonical block at slot " & $plan.slot.uint64 &
          " was not found")
    else:
      blockOpt = await fetchSszBlockByRootOptional(
        self.session, self.base, plan.root, self.spec)
      if blockOpt.isNone:
        stderr.writeLine "[fcr-nimbus] planned non-canonical block at slot " &
          $plan.slot.uint64 & " root 0x" & plan.root.data.toHex() &
          " was not found"
        continue

    let fetched = blockOpt.get
    let fetchedRoot = forkedBlockRoot(fetched)
    if fetchedRoot != plan.root:
      raise newException(EngineError,
        "planned block root mismatch at slot " & $plan.slot.uint64 &
        ": expected 0x" & plan.root.data.toHex() & ", got 0x" &
        fetchedRoot.data.toHex())
    let fetchedSlot = forkedBlockSlot(fetched)
    if fetchedSlot != plan.slot:
      raise newException(EngineError,
        "planned block slot mismatch for root 0x" & plan.root.data.toHex() &
        ": expected " & $plan.slot.uint64 & ", got " & $fetchedSlot.uint64)

    let imported = self.processBlock(fetched)
    if imported.isErr:
      if plan.canonical:
        raise newException(EngineError,
          "processBlock@" & $plan.slot.uint64 & ": " & imported.error)
      stderr.writeLine "[fcr-nimbus] skipping non-canonical block at slot " &
        $plan.slot.uint64 & " root 0x" & plan.root.data.toHex() & ": " &
        imported.error
      continue
    if not imported.value:
      stderr.writeLine "[fcr-nimbus] duplicate planned block at slot " &
        $plan.slot.uint64 & " root 0x" & plan.root.data.toHex()

  (hasBlock: blockRoot.isSome, blockRoot: blockRoot)

func planCheckpointToNative(planned: PlanCheckpoint): Checkpoint =
  Checkpoint(epoch: planned.epoch, root: planned.root)

func planAttestationDataToNative(planned: PlanAttestationData): AttestationData =
  AttestationData(
    slot: planned.slot,
    index: planned.index,
    beacon_block_root: planned.beaconBlockRoot,
    source: planCheckpointToNative(planned.source),
    target: planCheckpointToNative(planned.target))

proc decodeBaseAggregationBits(bytes: seq[byte]):
    Result[CommitteeValidatorsBits, string] =
  try:
    ok(SSZ.decode(bytes, CommitteeValidatorsBits))
  except CatchableError as e:
    err("failed to decode base aggregation_bits: " & e.msg)

proc decodeElectraAggregationBits(bytes: seq[byte]):
    Result[electra.AggregationBits, string] =
  try:
    ok(SSZ.decode(bytes, electra.AggregationBits))
  except CatchableError as e:
    err("failed to decode electra aggregation_bits: " & e.msg)

proc decodeElectraCommitteeBits(bytes: seq[byte]):
    Result[AttestationCommitteeBits, string] =
  try:
    ok(SSZ.decode(bytes, AttestationCommitteeBits))
  except CatchableError as e:
    err("failed to decode electra committee_bits: " & e.msg)

proc logFailedAttestationInjection(self: Engine, simSlot: Slot,
    data: AttestationData, reason: string) =
  let
    targetRoot = data.target.root
    headRoot = data.beacon_block_root
    targetInFc = self.attPool[].forkChoice.backend.contains(targetRoot)
    headInFc = self.attPool[].forkChoice.backend.contains(headRoot)
    finalized = self.attPool[].forkChoice.checkpoints.finalized
  stderr.writeLine "[fcr-nimbus] failed to inject attestation for sim_slot " &
    $simSlot.uint64 & " attestation_slot " & $data.slot.uint64 &
    " error " & reason &
    " target_root 0x" & targetRoot.data.toHex() &
    " target_in_fc " & $targetInFc &
    " head_root 0x" & headRoot.data.toHex() &
    " head_in_fc " & $headInFc &
    " finalized_epoch " & $finalized.epoch.uint64 &
    " finalized_root 0x" & finalized.root.data.toHex()

proc injectForkChoiceAttestation(self: Engine, simSlot: Slot,
    data: AttestationData, attestingIndices: seq[ValidatorIndex]):
    bool =
  let
    targetRoot = data.target.root
    headRoot = data.beacon_block_root
    targetInFc = self.attPool[].forkChoice.backend.contains(targetRoot)
    headInFc = self.attPool[].forkChoice.backend.contains(headRoot)
    injectSlot = simSlot + 1
    wallTime = injectSlot.start_beacon_time(self.dag.timeParams)

  # Lighthouse accepts zero-head attestations without applying them to fork
  # choice; do the same instead of letting Nimbus record a zero latest vote.
  if headRoot.isZero:
    return true

  if not targetInFc or not headInFc:
    let reason =
      if not targetInFc:
        "InvalidAttestation(UnknownTargetRoot)"
      else:
        "InvalidAttestation(UnknownHeadBlock)"
    self.logFailedAttestationInjection(simSlot, data, reason)
    return false

  let
    res = self.attPool[].forkChoice.on_attestation(
      self.dag, data.slot, data.beacon_block_root, attestingIndices, wallTime)
  if res.isErr:
    self.logFailedAttestationInjection(simSlot, data, $res.error)
    return false
  true

proc injectAttestations(self: Engine, simSlot: Slot,
    attestations: seq[PlanAttestation]): Result[uint64, string] =
  var injected: uint64 = 0
  for planned in attestations:
    let data = planAttestationDataToNative(planned.data)
    var cache = StateCache()
    let fork = self.spec.consensusForkAtEpoch(planned.data.slot.epoch)
    if fork >= ConsensusFork.Electra:
      if planned.committeeBits.isNone:
        return err("electra attestation is missing committee_bits")

      let aggregationBits = decodeElectraAggregationBits(planned.aggregationBits)
      if aggregationBits.isErr:
        return err(aggregationBits.error)
      let committeeBits = decodeElectraCommitteeBits(planned.committeeBits.get)
      if committeeBits.isErr:
        return err(committeeBits.error)

      let attestingIndices = withState(self.dag.headState):
        var indices: seq[ValidatorIndex]
        for vidx in forkyState.data.get_attesting_indices(
            data.slot, committeeBits.value, aggregationBits.value, cache):
          indices.add(vidx)
        indices
      if attestingIndices.len == 0:
        continue
      if self.injectForkChoiceAttestation(simSlot, data, attestingIndices):
        inc injected
    else:
      let aggregationBits = decodeBaseAggregationBits(planned.aggregationBits)
      if aggregationBits.isErr:
        return err(aggregationBits.error)

      let attestingIndices = withState(self.dag.headState):
        var indices: seq[ValidatorIndex]
        let
          committeesPerSlot = get_committee_count_per_slot(
            forkyState.data, data.slot.epoch, cache)
          committeeIndex = check_attestation_index(data, committeesPerSlot)
        if committeeIndex.isOk:
          for vidx in forkyState.data.get_attesting_indices(
              data.slot, committeeIndex.value, aggregationBits.value, cache):
            indices.add(vidx)
        indices
      if attestingIndices.len == 0:
        continue
      if self.injectForkChoiceAttestation(simSlot, data, attestingIndices):
        inc injected
  ok(injected)

proc recomputeHead(self: Engine, evalSlot: Slot): Result[Eth2Digest, string] =
  let wallTime = evalSlot.start_beacon_time(self.dag.cfg.timeParams)
  let headRes = self.attPool[].forkChoice.get_head(self.dag, wallTime)
  if headRes.isErr:
    return err("get_head failed")
  let head = headRes.value
  let headRefOpt = self.dag.getBlockRef(head)
  if headRefOpt.isNone:
    return err("getBlockRef(head) failed")
  let headRef = headRefOpt.get
  self.dag.updateHead(headRef, self.quarantine[], [])
  let wsRes = self.attPool[].forkChoice.will_select_head(self.dag, headRef, wallTime)
  if wsRes.isErr:
    return err("will_select_head failed")
  ok(head)

proc emitRecord(self: Engine, simSlot: Slot, hasBlock: bool,
    blockRoot: Option[Eth2Digest], headRoot: Eth2Digest,
    numInjected: uint64, fcrEvalUs: uint64) =
  let confirmedBid = self.attPool[].forkChoice.retrieve_fast_confirmed_bid()
  let confirmedRoot = confirmedBid.root
  let confirmedSlot = confirmedBid.slot.uint64
  let zeroRoot = default(Eth2Digest)
  let fastConfirmed =
    confirmedRoot != zeroRoot and confirmedSlot == simSlot.uint64
  let eval = simSlot.uint64 + 1
  let delay = if eval >= confirmedSlot: eval - confirmedSlot else: 0
  let strictOne = hasBlock and confirmedRoot != zeroRoot and
    blockRoot.isSome and blockRoot.get == confirmedRoot and
    confirmedSlot == simSlot.uint64 and delay == 1

  let finalizedEpoch = self.dag.headState.finalized_checkpoint.epoch.uint64
  let justifiedEpoch = self.dag.headState.current_justified_checkpoint.epoch.uint64

  var rec = newJObject()
  rec["slot"] = %(simSlot.uint64)
  rec["epoch"] = %(simSlot.uint64 div 32)
  rec["has_block"] = %hasBlock
  if blockRoot.isSome:
    rec["block_root"] = %("0x" & blockRoot.get.data.toHex())
  else:
    rec["block_root"] = newJNull()
  rec["head_root"] = %("0x" & headRoot.data.toHex())
  rec["confirmed_root"] = %("0x" & confirmedRoot.data.toHex())
  rec["confirmed_slot"] = %confirmedSlot
  rec["confirmation_delay_slots"] = %delay
  rec["fast_confirmed"] = %fastConfirmed
  rec["strict_one_slot_confirmed"] = %strictOne
  rec["finalized_epoch"] = %finalizedEpoch
  rec["justified_epoch"] = %justifiedEpoch
  rec["source_block_slot"] = newJNull()
  rec["num_attestations_injected"] = %numInjected
  rec["is_epoch_boundary"] = %((simSlot.uint64 mod 32) == 0)
  rec["is_missed_slot"] = %(not hasBlock)
  rec["fcr_eval_duration_us"] = %fcrEvalUs

  self.outFile.write($rec)
  self.outFile.write("\n")
  inc self.recordsWritten
  if self.recordsWritten mod FlushInterval == 0:
    self.outFile.flushFile()

proc run(self: Engine): Future[Result[void, string]]
    {.async: (raises: [CatchableError]).} =
  var slot = self.cfg.warmupStartSlot + 1
  while slot < self.cfg.endSlot:
    let isRecording = slot >= self.cfg.startSlot

    let instruction = await fetchSlotInstruction(
      self.session, self.base, slot, self.cfg.warmupStartSlot)
    if instruction.simSlot != slot:
      return err("slot instruction response sim_slot mismatch: requested " &
        $slot.uint64 & ", got " & $instruction.simSlot.uint64)

    let importedBlocks = await self.importPlannedBlocks(
      slot, instruction.importBlocks)

    let injected = self.injectAttestations(slot, instruction.attestations)
    if injected.isErr:
      return err("injectAttestations@" & $slot.uint64 & ": " & injected.error)
    let numInjected = injected.value

    let t0 = getMonoTime()
    let headRes = self.recomputeHead(instruction.evalSlot)
    if headRes.isErr:
      return err("recomputeHead@" & $slot.uint64 & ": " & results.error(headRes))
    let dur = (getMonoTime() - t0).inMicroseconds.uint64

    if isRecording:
      self.emitRecord(slot, importedBlocks.hasBlock, importedBlocks.blockRoot,
        headRes.value, numInjected, dur)

    slot = slot + 1
  self.outFile.flushFile()
  self.outFile.close()
  ok()

# --------------------------------------------------------------------------------
# Entry point

proc main() {.async: (raises: [CatchableError]).} =
  let (cfg, exit) = parseEngineConfig()
  if exit.isSome:
    quit(exit.get.int)
  if cfg.manifestJson:
    printManifest()
    quit(0)
  var engine = try:
    await Engine.init(cfg)
  except CatchableError as e:
    stderr.writeLine "bootstrap failure: " & e.msg
    quit(int(ekBootstrap))
  let res =
    try: await engine.run()
    except CatchableError as e:
      stderr.writeLine "simulation failure: " & e.msg
      quit(int(ekRunFailure))
  if res.isErr:
    stderr.writeLine "simulation failure: " & res.error
    quit(int(ekRunFailure))

when isMainModule:
  waitFor main()
