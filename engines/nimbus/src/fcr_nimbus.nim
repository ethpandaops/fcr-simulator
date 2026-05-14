# fcr-nimbus: Nimbus FCR replay engine for fcr-simulator.
#
# Implements the engine contract documented in fcr-simulator/docs/ENGINE_CONTRACT.md.
# Drives nimbus-eth2's headless ChainDAGRef + AttestationPool fork choice across a
# warmup/recording slot range, emitting JSONL records that the Go orchestrator merges
# into the final per-run CSV.

{.push raises: [].}

import
  std/[json, options, os, parseopt, strformat, strutils, tables, times],
  chronos,
  chronos/apps/http/httpclient,
  results,
  stew/[byteutils, io2],
  taskpools,
  ../nimbus-eth2/beacon_chain/[beacon_chain_db, beacon_clock, conf,
    validator_pool],
  ../nimbus-eth2/beacon_chain/consensus_object_pools/[
    attestation_pool, blockchain_dag, block_clearance, block_quarantine,
    spec_cache],
  ../nimbus-eth2/beacon_chain/fork_choice/[fork_choice, fork_choice_types],
  ../nimbus-eth2/beacon_chain/gossip_processing/batch_validation,
  ../nimbus-eth2/beacon_chain/spec/[
    beaconstate, forks, helpers, presets, signatures, state_transition],
  ../nimbus-eth2/beacon_chain/spec/datatypes/[base, phase0, altair, bellatrix,
    capella, deneb, electra],
  ../nimbus-eth2/beacon_chain/validators/validator_monitor

from ../nimbus-eth2/beacon_chain/spec/eth2_apis/eth2_rest_serialization import
  RestApiResponse

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

  PlanEntry = object
    simSlot: uint64
    sourceBlockSlot: Option[uint64]

  AttestationPlan = Table[uint64, Option[uint64]]

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
  var parser = initOptParser(quoteShellCommand(commandLineParams()))
  for kind, key, val in parser.getopt():
    case kind
    of cmdLongOption:
      case key
      of "beacon-node-url": cfg.beaconNodeUrl = val
      of "start-slot":
        try: cfg.startSlot = Slot(parseBiggestUInt(val))
        except ValueError:
          stderr.writeLine "invalid --start-slot: " & val
          return (cfg, some(ekConfig))
      of "end-slot":
        try: cfg.endSlot = Slot(parseBiggestUInt(val))
        except ValueError:
          stderr.writeLine "invalid --end-slot: " & val
          return (cfg, some(ekConfig))
      of "warmup-start-slot":
        try: cfg.warmupStartSlot = Slot(parseBiggestUInt(val))
        except ValueError:
          stderr.writeLine "invalid --warmup-start-slot: " & val
          return (cfg, some(ekConfig))
      of "network": cfg.network = val
      of "byzantine-threshold":
        try: cfg.byzantineThreshold = parseBiggestUInt(val).uint64
        except ValueError:
          stderr.writeLine "invalid --byzantine-threshold: " & val
          return (cfg, some(ekConfig))
      of "attestation-source-mode": cfg.attestationSourceMode = val
      of "lookahead-cap":
        try: cfg.lookaheadCap = parseBiggestUInt(val).uint64
        except ValueError:
          stderr.writeLine "invalid --lookahead-cap: " & val
          return (cfg, some(ekConfig))
      of "output": cfg.output = val
      of "manifest-json": cfg.manifestJson = true
      else:
        stderr.writeLine "unknown flag --" & key
        return (cfg, some(ekConfig))
    of cmdArgument:
      stderr.writeLine "unexpected positional argument: " & key
      return (cfg, some(ekConfig))
    else: discard

  if cfg.manifestJson:
    return (cfg, none(ExitKind))

  if cfg.beaconNodeUrl.len == 0:
    stderr.writeLine "missing --beacon-node-url"
    return (cfg, some(ekConfig))
  if cfg.network != "mainnet":
    stderr.writeLine "--network must be 'mainnet' (V1)"
    return (cfg, some(ekConfig))
  if cfg.attestationSourceMode notin [
      "next-non-missed", "strict-source-block-k-minus-1"]:
    stderr.writeLine "invalid --attestation-source-mode"
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

proc httpGet(session: HttpSessionRef, url: string, acceptSsz: bool):
    Future[tuple[status: int, headers: HttpResponseHeaderRef, body: seq[byte]]]
    {.async: (raises: [CatchableError]).} =
  let uri = parseUri(url).valueOr:
    raise newException(EngineError, "bad URL: " & url)
  let req = HttpClientRequestRef.new(
    session, uri, HttpMethod.MethodGet,
    headers = if acceptSsz: @[
        HttpHeaderTuple(key: "Accept", value: "application/octet-stream"),
      ] else: @[
        HttpHeaderTuple(key: "Accept", value: "application/json"),
      ]).valueOr:
    raise newException(EngineError, "bad request for " & url)
  let resp = await req.send()
  let body = await resp.getBodyBytes()
  result = (status: resp.status.int, headers: resp.headers, body: body)
  await resp.closeWait()

proc fetchSszBlockAtSlot(session: HttpSessionRef, base: string, slot: Slot,
    cfg: RuntimeConfig):
    Future[Option[ForkedSignedBeaconBlock]]
    {.async: (raises: [CatchableError]).} =
  let url = base & "/eth/v2/beacon/blocks/" & $slot.uint64
  let (status, _, body) = await httpGet(session, url, acceptSsz = true)
  if status == 404:
    return none(ForkedSignedBeaconBlock)
  if status != 200:
    raise newException(EngineError, &"HTTP {status} from {url}")
  let blck = readSszForkedSignedBeaconBlock(cfg, body)
  some(blck)

proc fetchSszBlockByRoot(session: HttpSessionRef, base: string,
    root: Eth2Digest, cfg: RuntimeConfig):
    Future[ForkedSignedBeaconBlock] {.async: (raises: [CatchableError]).} =
  let url = base & "/eth/v2/beacon/blocks/" & root.data.toHex(true)
  let (status, _, body) = await httpGet(session, url, acceptSsz = true)
  if status != 200:
    raise newException(EngineError, &"HTTP {status} from {url}")
  readSszForkedSignedBeaconBlock(cfg, body)

proc fetchCheckpointState(session: HttpSessionRef, base: string, slot: Slot,
    cfg: RuntimeConfig):
    Future[ForkedHashedBeaconState] {.async: (raises: [CatchableError]).} =
  let url = base & "/eth/v2/debug/beacon/states/" & $slot.uint64
  let (status, _, body) = await httpGet(session, url, acceptSsz = true)
  if status != 200:
    raise newException(EngineError, &"HTTP {status} from {url}")
  readSszForkedHashedBeaconState(cfg, body)

proc fetchGenesisState(session: HttpSessionRef, base: string,
    cfg: RuntimeConfig):
    Future[ForkedHashedBeaconState] {.async: (raises: [CatchableError]).} =
  let url = base & "/eth/v2/debug/beacon/states/genesis"
  let (status, _, body) = await httpGet(session, url, acceptSsz = true)
  if status != 200:
    raise newException(EngineError, &"HTTP {status} from {url}")
  readSszForkedHashedBeaconState(cfg, body)

proc fetchAttestationPlan(session: HttpSessionRef, base: string,
    fromSlot, toSlot: Slot):
    Future[AttestationPlan] {.async: (raises: [CatchableError]).} =
  let url =
    base & "/fcr-sim/v1/plan?from=" & $fromSlot.uint64 & "&to=" & $toSlot.uint64
  let (status, _, body) = await httpGet(session, url, acceptSsz = false)
  if status != 200:
    raise newException(EngineError, &"HTTP {status} from {url}")
  let bodyStr = string.fromBytes(body)
  let parsed = parseJson(bodyStr)
  var plan: AttestationPlan
  for entry in parsed["entries"]:
    let sim = entry["sim_slot"].getInt().uint64
    if entry.hasKey("source_block_slot") and entry["source_block_slot"].kind != JNull:
      plan[sim] = some(entry["source_block_slot"].getInt().uint64)
    else:
      plan[sim] = none(uint64)
  plan

# --------------------------------------------------------------------------------
# Engine core

type
  Engine = object
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
    plan: AttestationPlan
    blockCache: Table[uint64, Option[ForkedSignedBeaconBlock]]
    out: File
    recordsWritten: uint64

proc mainnetSpec(byzantineThreshold: uint64): RuntimeConfig =
  result = defaultRuntimeConfig
  result.CONFIRMATION_BYZANTINE_THRESHOLD = byzantineThreshold

proc init(T: type Engine, cfg: EngineConfig): Future[T]
    {.async: (raises: [CatchableError]).} =
  var eng: Engine
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

  let memDb = BeaconChainDB.new("", inMemory = true)
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

  stderr.writeLine "[fcr-nimbus] fetching attestation plan from " &
    $cfg.warmupStartSlot.uint64 & " (+1) to " & $cfg.endSlot.uint64
  eng.plan = await fetchAttestationPlan(
    eng.session, eng.base, cfg.warmupStartSlot + 1, cfg.endSlot)

  # Open output file (overwrite mode).
  eng.out = open(cfg.output, fmWrite)

  return eng

proc getBlockAtSlot(self: var Engine, slot: Slot):
    Future[Option[ForkedSignedBeaconBlock]]
    {.async: (raises: [CatchableError]).} =
  let key = slot.uint64
  if key in self.blockCache:
    return self.blockCache[key]
  let blck = await fetchSszBlockAtSlot(
    self.session, self.base, slot, self.spec)
  self.blockCache[key] = blck
  blck

proc processBlock(self: var Engine, forked: ForkedSignedBeaconBlock):
    Result[BlockRef, string] =
  ## Imports a canonical-chain block via dag.addHeadBlockWithParent + addForkChoice.
  let checked = withBlck(forked):
    let parent = checkHeadBlock(self.dag, forkyBlck).valueOr:
      if error == VerifierError.Duplicate:
        # Same block already in dag (e.g. cached); proceed without re-import.
        return ok(self.dag.getBlockRef(forkyBlck.root).valueOr:
          return err("Duplicate but missing BlockRef"))
      return err("checkHeadBlock failed: " & $error)
    let addRes = addHeadBlockWithParent(
      self.dag, self.batchVerifier[], forkyBlck, parent,
      OptimisticStatus.valid,
      onBlockAdded = OnBlockAdded(nil))
    if addRes.isErr:
      return err("addHeadBlock failed: " & $addRes.error)
    let blckRef = addRes.value
    let epochRef = self.dag.getEpochRef(blckRef, blckRef.slot.epoch, false).valueOr:
      return err("getEpochRef failed: " & $error)
    let unrealized = withState(self.dag.headState):
      when consensusFork >= ConsensusFork.Altair:
        let (cps, _) = forkyState.data.compute_unrealized_finality()
        cps
      else:
        default(FinalityCheckpoints)
    self.attPool[].addForkChoice(
      epochRef, blckRef, unrealized, forkyBlck.message,
      blckRef.slot.start_beacon_time(self.dag.timeParams))
    blckRef
  ok(checked)

proc injectAttestationsFromBlock(self: var Engine, simSlot: Slot,
    sourceBlockSlot: Slot): Future[uint64]
    {.async: (raises: [CatchableError]).} =
  let blckOpt = await self.getBlockAtSlot(sourceBlockSlot)
  if blckOpt.isNone:
    raise newException(EngineError,
      "attestation plan referenced missing source block at slot " &
      $sourceBlockSlot.uint64)
  let injectSlot = simSlot + 1
  let wallTime = injectSlot.start_beacon_time(self.dag.timeParams)
  var injected: uint64 = 0
  withBlck(blckOpt.get()):
    for attestation in forkyBlck.message.body.attestations:
      var attestingIndices: seq[ValidatorIndex]
      for vidx in self.dag.get_attesting_indices(attestation):
        attestingIndices.add(vidx)
      if attestingIndices.len == 0:
        continue
      let res = self.attPool[].forkChoice.on_attestation(
        self.dag,
        attestation.data.slot,
        attestation.data.beacon_block_root,
        attestingIndices,
        wallTime)
      if res.isOk:
        inc injected
  injected

proc recomputeHead(self: var Engine, simSlot: Slot): Result[Eth2Digest, string] =
  let wallTime = (simSlot + 1).start_beacon_time(self.dag.timeParams)
  let head = self.attPool[].forkChoice.get_head(self.dag, wallTime).valueOr:
    return err("get_head failed: " & $error)
  let headRef = self.dag.getBlockRef(head).valueOr:
    return err("getBlockRef(head) failed: " & $error)
  self.dag.updateHead(headRef, self.quarantine[], [])
  # will_select_head triggers FCR advance + confirmed root update.
  self.attPool[].forkChoice.will_select_head(self.dag, headRef, wallTime).isOkOr:
    return err("will_select_head failed: " & $error)
  ok(head)

proc emitRecord(self: var Engine, simSlot: Slot, hasBlock: bool,
    blockRoot: Option[Eth2Digest], headRoot: Eth2Digest,
    sourceSlot: Option[Slot], numInjected: uint64,
    fcrEvalUs: uint64) =
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

  let head = self.dag.head
  let finalizedEpoch =
    self.attPool[].forkChoice.checkpoints.finalized.epoch.uint64
  let justifiedEpoch =
    self.attPool[].forkChoice.checkpoints.justified.checkpoint.epoch.uint64

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
  if sourceSlot.isSome:
    rec["source_block_slot"] = %(sourceSlot.get.uint64)
  else:
    rec["source_block_slot"] = newJNull()
  rec["num_attestations_injected"] = %numInjected
  rec["is_epoch_boundary"] = %((simSlot.uint64 mod 32) == 0)
  rec["is_missed_slot"] = %(not hasBlock)
  rec["fcr_eval_duration_us"] = %fcrEvalUs

  self.out.write($rec)
  self.out.write("\n")
  inc self.recordsWritten
  if self.recordsWritten mod FlushInterval == 0:
    self.out.flushFile()

proc run(self: var Engine): Future[Result[void, string]]
    {.async: (raises: [CatchableError]).} =
  var slot = self.cfg.warmupStartSlot + 1
  while slot < self.cfg.endSlot:
    if not self.plan.hasKey(slot.uint64):
      return err("attestation plan missing sim_slot " & $slot.uint64)
    let isRecording = slot >= self.cfg.startSlot

    let blockOpt = await self.getBlockAtSlot(slot)
    let hasBlock = blockOpt.isSome
    var blockRoot = none(Eth2Digest)
    if hasBlock:
      let imported = self.processBlock(blockOpt.get())
      if imported.isErr:
        return err("processBlock@" & $slot.uint64 & ": " & imported.error)
      blockRoot = some(imported.value.root)

    let sourceOpt = self.plan[slot.uint64]
    var sourceSlot = none(Slot)
    var numInjected: uint64 = 0
    if sourceOpt.isSome:
      sourceSlot = some(Slot(sourceOpt.get))
      numInjected = await self.injectAttestationsFromBlock(slot, sourceSlot.get)

    let t0 = getMonoTime()
    let headRes = self.recomputeHead(slot)
    if headRes.isErr:
      return err("recomputeHead@" & $slot.uint64 & ": " & headRes.error)
    let dur = (getMonoTime() - t0).inMicroseconds.uint64

    if isRecording:
      self.emitRecord(slot, hasBlock, blockRoot, headRes.value,
        sourceSlot, numInjected, dur)

    slot = slot + 1
  self.out.flushFile()
  self.out.close()
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

{.pop.}
