export type JevStatus = "running" | "disabled" | "degraded" | "stale";
export type JevModelStatus = "not_called" | "success" | "error" | "unauthorized";

export interface JevSingleFilterStats {
  input: number;
  contractPassed: number;
  positive24hPassed: number;
  freshPassed: number;
  turnoverPassed: number;
  spotMatched: number;
  bboFreshPassed: number;
  depthPassed: number;
  basisForecastPassed: number;
  final: number;
}

export interface JevCrossFilterStats {
  input: number;
  contractPassed: number;
  positiveEdgePassed: number;
  freshPassed: number;
  turnoverPassed: number;
  bboFreshPassed: number;
  depthPassed: number;
  basisForecastPassed: number;
  final: number;
}

export interface JevSnapshot {
  status: JevStatus | string;
  degradedReasons: string[];
  lastSuccessfulAt: string;
  lastDecisionAt: string;
  lastMarketDataAt: string;
  serverTime: string;
  model: string;
  modelRequested: string;
  promptVersion: string;
  policyVersion: string;
  simulated: boolean;
  equity: string;
  netPnl: string;
  fundingPnl: string;
  realizedSpreadPnl: string;
  unrealizedSpreadPnl: string;
  cumulativeCost: string;
  maxDrawdown: string;
  initialEquity: string;
  paperRoundTripCostBps: string;
  usdtUsdcOneToOne: boolean;
  singleEntryEmptyReason: string | null;
  crossEntryEmptyReason: string | null;
  strategySingleStatus: string;
  strategyCrossStatus: string;
  openPositionCount: number;
  entryCandidateCount: number;
  typesafeConfigured: boolean;
  snapshotVersion: string;
  singleFilter: JevSingleFilterStats;
  crossFilter: JevCrossFilterStats;
  decisionJobs: number;
  typesafeRequests: number;
  typesafeSuccess: number;
  typesafeError: number;
  modelStatus: JevModelStatus | string;
  lastModelCallAt: string;
  strategyVersion: string;
  lifecycleEquity: string;
  lifecycleNetPnl: string;
  lifecycleFundingPnl: string;
  lifecycleRealizedSpreadPnl: string;
  lifecycleUnrealizedSpreadPnl: string;
  lifecycleCumulativeCost: string;
  availableStrategyVersions: string[];
}

export interface JevPosition {
  id: string;
  kind: string;
  status: string;
  candidateKey: string;
  baseAsset: string;
  quoteAsset: string;
  globalSymbol: string;
  longExchange: string;
  longSymbol: string;
  longProduct: string;
  shortExchange: string;
  shortSymbol: string;
  shortProduct: string;
  sharedBaseQty: string;
  longQty: string;
  shortQty: string;
  longEntryPrice: string;
  shortEntryPrice: string;
  longMarkPrice: string;
  shortMarkPrice: string;
  realizedSpreadPnl: string;
  unrealizedSpreadPnl: string;
  fundingPnl: string;
  costPaid: string;
  netPnl: string;
  exitPendingReason: string | null;
  degradedReason: string | null;
  openedAt: string;
  closedAt: string;
  lastMarkAt: string;
  entryMidBasisBps: string | null;
  strategyVersion: string | null;
  netIfExit: string | null;
  netIfExitAt: string | null;
  exitConfirmCount: number;
}

export interface JevDecision {
  id: string;
  positionId: string | null;
  candidateKey: string;
  kind: string;
  decision: string;
  policyStatus: string;
  executionStatus: string;
  enterProbability: string;
  exitProbability: string;
  confidence: string;
  expectedNetReturn24h: string | null;
  fundingReturn24h: string | null;
  holdingHorizonNetReturn: string | null;
  expectedPaybackMinutes: string | null;
  currentMidBasisBps: string | null;
  entryMidBasisBps: string | null;
  basisChangeSinceEntryBps: string | null;
  expectedBasisReturn24h: string | null;
  basisReturnP10: string | null;
  basisProfitProbability: string | null;
  basisForecastConfidence: string | null;
  basisSampleCount: number | null;
  basisModelVersion: string | null;
  rationale: string;
  promptVersion: string;
  policyVersion: string;
  modelRequested: string;
  modelActual: string;
  errorMessage: string | null;
  decisionAt: string;
  executionStartedAt: string;
  fillDeadline: string;
  strategyVersion: string | null;
  parseErrorType: string | null;
}

export interface JevTrade {
  id: string;
  positionId: string;
  decisionId: string | null;
  side: string;
  sharedBaseQty: string;
  longFillPrice: string;
  shortFillPrice: string;
  notional: string;
  cost: string;
  filledAt: string;
  strategyVersion: string | null;
  baseAsset: string | null;
  kind: string | null;
  longExchange: string | null;
  longSymbol: string | null;
  longProduct: string | null;
  shortExchange: string | null;
  shortSymbol: string | null;
  shortProduct: string | null;
}

export interface JevEquityCurve {
  range: string;
  points: Array<{ ts: string; equity: string; netPnl: string }>;
}

export type JevFetchResult<T> =
  | { status: "unchanged"; etag: string | null }
  | { status: "updated"; data: T; etag: string | null };

function apiBaseUrl(): string {
  return (process.env.NEXT_PUBLIC_API_BASE_URL ?? "").replace(/\/+$/, "");
}

function jevUrl(path: string): string {
  return `${apiBaseUrl()}/api/v1/jev-arb${path}`;
}

function text(value: unknown, field: string): string {
  if (value === null || value === undefined) {
    return "";
  }
  if (typeof value === "string" || typeof value === "number" || typeof value === "boolean") {
    return String(value);
  }
  throw new Error(`${field} 必须是标量`);
}

function optionalText(value: unknown): string | null {
  if (value === null || value === undefined || value === "") {
    return null;
  }
  return text(value, "optional");
}

function bool(value: unknown, field: string): boolean {
  if (typeof value === "boolean") return value;
  throw new Error(`${field} 必须是 boolean`);
}

function integer(value: unknown, field: string): number {
  const parsed = Number(value);
  if (!Number.isFinite(parsed)) {
    throw new Error(`${field} 必须是数字`);
  }
  return parsed;
}

function strings(value: unknown, field: string): string[] {
  if (value === undefined || value === null) return [];
  if (!Array.isArray(value)) {
    throw new Error(`${field} 必须是数组`);
  }
  return value.map((item, index) => text(item, `${field}[${index}]`));
}

export function mapJevSnapshot(raw: unknown): JevSnapshot {
  if (raw === null || typeof raw !== "object") {
    throw new Error("JEV snapshot 无效");
  }
  const item = raw as Record<string, unknown>;
  return {
    status: text(item.status, "status"),
    degradedReasons: strings(item.degradedReasons, "degradedReasons"),
    lastSuccessfulAt: text(item.lastSuccessfulAt, "lastSuccessfulAt"),
    lastDecisionAt: text(item.lastDecisionAt, "lastDecisionAt"),
    lastMarketDataAt: text(item.lastMarketDataAt, "lastMarketDataAt"),
    serverTime: text(item.serverTime, "serverTime"),
    model: text(item.model, "model"),
    modelRequested: text(item.modelRequested, "modelRequested"),
    promptVersion: text(item.promptVersion, "promptVersion"),
    policyVersion: text(item.policyVersion, "policyVersion"),
    simulated: bool(item.simulated, "simulated"),
    equity: text(item.equity, "equity"),
    netPnl: text(item.netPnl, "netPnl"),
    fundingPnl: text(item.fundingPnl, "fundingPnl"),
    realizedSpreadPnl: text(item.realizedSpreadPnl, "realizedSpreadPnl"),
    unrealizedSpreadPnl: text(item.unrealizedSpreadPnl, "unrealizedSpreadPnl"),
    cumulativeCost: text(item.cumulativeCost, "cumulativeCost"),
    maxDrawdown: text(item.maxDrawdown, "maxDrawdown"),
    initialEquity: text(item.initialEquity, "initialEquity"),
    paperRoundTripCostBps: text(item.paperRoundTripCostBps, "paperRoundTripCostBps"),
    usdtUsdcOneToOne: bool(item.usdtUsdcOneToOne, "usdtUsdcOneToOne"),
    singleEntryEmptyReason: optionalText(item.singleEntryEmptyReason),
    crossEntryEmptyReason: optionalText(item.crossEntryEmptyReason),
    strategySingleStatus: text(item.strategySingleStatus, "strategySingleStatus"),
    strategyCrossStatus: text(item.strategyCrossStatus, "strategyCrossStatus"),
    openPositionCount: integer(item.openPositionCount, "openPositionCount"),
    entryCandidateCount: integer(item.entryCandidateCount, "entryCandidateCount"),
    typesafeConfigured: bool(item.typesafeConfigured, "typesafeConfigured"),
    snapshotVersion: text(item.snapshotVersion, "snapshotVersion"),
    singleFilter: mapSingleFilter(item.singleFilter),
    crossFilter: mapCrossFilter(item.crossFilter),
    decisionJobs: optionalInteger(item.decisionJobs),
    typesafeRequests: optionalInteger(item.typesafeRequests),
    typesafeSuccess: optionalInteger(item.typesafeSuccess),
    typesafeError: optionalInteger(item.typesafeError),
    modelStatus: text(item.modelStatus ?? "not_called", "modelStatus") || "not_called",
    lastModelCallAt: text(item.lastModelCallAt ?? "", "lastModelCallAt"),
    strategyVersion: text(item.strategyVersion ?? "", "strategyVersion"),
    lifecycleEquity: text(item.lifecycleEquity ?? item.equity, "lifecycleEquity"),
    lifecycleNetPnl: text(item.lifecycleNetPnl ?? item.netPnl, "lifecycleNetPnl"),
    lifecycleFundingPnl: text(item.lifecycleFundingPnl ?? item.fundingPnl, "lifecycleFundingPnl"),
    lifecycleRealizedSpreadPnl: text(item.lifecycleRealizedSpreadPnl ?? item.realizedSpreadPnl, "lifecycleRealizedSpreadPnl"),
    lifecycleUnrealizedSpreadPnl: text(item.lifecycleUnrealizedSpreadPnl ?? item.unrealizedSpreadPnl, "lifecycleUnrealizedSpreadPnl"),
    lifecycleCumulativeCost: text(item.lifecycleCumulativeCost ?? item.cumulativeCost, "lifecycleCumulativeCost"),
    availableStrategyVersions: strings(item.availableStrategyVersions, "availableStrategyVersions"),
  };
}

function optionalInteger(value: unknown): number {
  if (value === undefined || value === null || value === "") {
    return 0;
  }
  return integer(value, "count");
}

function optionalCount(value: unknown): number | null {
  if (value === undefined || value === null || value === "") {
    return null;
  }
  return integer(value, "count");
}

function mapSingleFilter(raw: unknown): JevSingleFilterStats {
  const item = raw && typeof raw === "object" ? (raw as Record<string, unknown>) : {};
  return {
    input: optionalInteger(item.input),
    contractPassed: optionalInteger(item.contractPassed),
    positive24hPassed: optionalInteger(item.positive24hPassed),
    freshPassed: optionalInteger(item.freshPassed),
    turnoverPassed: optionalInteger(item.turnoverPassed),
    spotMatched: optionalInteger(item.spotMatched),
    bboFreshPassed: optionalInteger(item.bboFreshPassed),
    depthPassed: optionalInteger(item.depthPassed),
    basisForecastPassed: optionalInteger(item.basisForecastPassed),
    final: optionalInteger(item.final),
  };
}

function mapCrossFilter(raw: unknown): JevCrossFilterStats {
  const item = raw && typeof raw === "object" ? (raw as Record<string, unknown>) : {};
  return {
    input: optionalInteger(item.input),
    contractPassed: optionalInteger(item.contractPassed),
    positiveEdgePassed: optionalInteger(item.positiveEdgePassed),
    freshPassed: optionalInteger(item.freshPassed),
    turnoverPassed: optionalInteger(item.turnoverPassed),
    bboFreshPassed: optionalInteger(item.bboFreshPassed),
    depthPassed: optionalInteger(item.depthPassed),
    basisForecastPassed: optionalInteger(item.basisForecastPassed),
    final: optionalInteger(item.final),
  };
}

export function mapJevPosition(raw: unknown, index: number): JevPosition {
  if (raw === null || typeof raw !== "object") {
    throw new Error(`positions[${index}] 无效`);
  }
  const item = raw as Record<string, unknown>;
  return {
    id: text(item.id, "id"),
    kind: text(item.kind, "kind"),
    status: text(item.status, "status"),
    candidateKey: text(item.candidateKey, "candidateKey"),
    baseAsset: text(item.baseAsset, "baseAsset"),
    quoteAsset: text(item.quoteAsset, "quoteAsset"),
    globalSymbol: text(item.globalSymbol, "globalSymbol"),
    longExchange: text(item.longExchange, "longExchange"),
    longSymbol: text(item.longSymbol, "longSymbol"),
    longProduct: text(item.longProduct, "longProduct"),
    shortExchange: text(item.shortExchange, "shortExchange"),
    shortSymbol: text(item.shortSymbol, "shortSymbol"),
    shortProduct: text(item.shortProduct, "shortProduct"),
    sharedBaseQty: text(item.sharedBaseQty, "sharedBaseQty"),
    longQty: text(item.longQty, "longQty"),
    shortQty: text(item.shortQty, "shortQty"),
    longEntryPrice: text(item.longEntryPrice, "longEntryPrice"),
    shortEntryPrice: text(item.shortEntryPrice, "shortEntryPrice"),
    longMarkPrice: text(item.longMarkPrice, "longMarkPrice"),
    shortMarkPrice: text(item.shortMarkPrice, "shortMarkPrice"),
    realizedSpreadPnl: text(item.realizedSpreadPnl, "realizedSpreadPnl"),
    unrealizedSpreadPnl: text(item.unrealizedSpreadPnl, "unrealizedSpreadPnl"),
    fundingPnl: text(item.fundingPnl, "fundingPnl"),
    costPaid: text(item.costPaid, "costPaid"),
    netPnl: text(item.netPnl, "netPnl"),
    exitPendingReason: optionalText(item.exitPendingReason),
    degradedReason: optionalText(item.degradedReason),
    openedAt: text(item.openedAt, "openedAt"),
    closedAt: text(item.closedAt, "closedAt"),
    lastMarkAt: text(item.lastMarkAt, "lastMarkAt"),
    entryMidBasisBps: optionalText(item.entryMidBasisBps),
    strategyVersion: optionalText(item.strategyVersion),
    netIfExit: optionalText(item.netIfExit),
    netIfExitAt: optionalText(item.netIfExitAt),
    exitConfirmCount: optionalInteger(item.exitConfirmCount),
  };
}

export function mapJevDecision(raw: unknown, index: number): JevDecision {
  if (raw === null || typeof raw !== "object") {
    throw new Error(`decisions[${index}] 无效`);
  }
  const item = raw as Record<string, unknown>;
  return {
    id: text(item.id, "id"),
    positionId: optionalText(item.positionId),
    candidateKey: text(item.candidateKey, "candidateKey"),
    kind: text(item.kind, "kind"),
    decision: text(item.decision, "decision"),
    policyStatus: text(item.policyStatus, "policyStatus"),
    executionStatus: text(item.executionStatus, "executionStatus"),
    enterProbability: text(item.enterProbability, "enterProbability"),
    exitProbability: text(item.exitProbability, "exitProbability"),
    confidence: text(item.confidence, "confidence"),
    expectedNetReturn24h: optionalText(item.expectedNetReturn24h),
    fundingReturn24h: optionalText(item.fundingReturn24h),
    holdingHorizonNetReturn: optionalText(item.holdingHorizonNetReturn),
    expectedPaybackMinutes: optionalText(item.expectedPaybackMinutes),
    currentMidBasisBps: optionalText(item.currentMidBasisBps),
    entryMidBasisBps: optionalText(item.entryMidBasisBps),
    basisChangeSinceEntryBps: optionalText(item.basisChangeSinceEntryBps),
    expectedBasisReturn24h: optionalText(item.expectedBasisReturn24h),
    basisReturnP10: optionalText(item.basisReturnP10),
    basisProfitProbability: optionalText(item.basisProfitProbability),
    basisForecastConfidence: optionalText(item.basisForecastConfidence),
    basisSampleCount: optionalCount(item.basisSampleCount),
    basisModelVersion: optionalText(item.basisModelVersion),
    rationale: text(item.rationale, "rationale"),
    promptVersion: text(item.promptVersion, "promptVersion"),
    policyVersion: text(item.policyVersion, "policyVersion"),
    modelRequested: text(item.modelRequested, "modelRequested"),
    modelActual: text(item.modelActual, "modelActual"),
    errorMessage: optionalText(item.errorMessage),
    decisionAt: text(item.decisionAt, "decisionAt"),
    executionStartedAt: text(item.executionStartedAt, "executionStartedAt"),
    fillDeadline: text(item.fillDeadline, "fillDeadline"),
    strategyVersion: optionalText(item.strategyVersion),
    parseErrorType: optionalText(item.parseErrorType),
  };
}

export function mapJevTrade(raw: unknown, index: number): JevTrade {
  if (raw === null || typeof raw !== "object") {
    throw new Error(`trades[${index}] 无效`);
  }
  const item = raw as Record<string, unknown>;
  return {
    id: text(item.id, "id"),
    positionId: text(item.positionId, "positionId"),
    decisionId: optionalText(item.decisionId),
    side: text(item.side, "side"),
    sharedBaseQty: text(item.sharedBaseQty, "sharedBaseQty"),
    longFillPrice: text(item.longFillPrice, "longFillPrice"),
    shortFillPrice: text(item.shortFillPrice, "shortFillPrice"),
    notional: text(item.notional, "notional"),
    cost: text(item.cost, "cost"),
    filledAt: text(item.filledAt, "filledAt"),
    strategyVersion: optionalText(item.strategyVersion),
    baseAsset: optionalText(item.baseAsset),
    kind: optionalText(item.kind),
    longExchange: optionalText(item.longExchange),
    longSymbol: optionalText(item.longSymbol),
    longProduct: optionalText(item.longProduct),
    shortExchange: optionalText(item.shortExchange),
    shortSymbol: optionalText(item.shortSymbol),
    shortProduct: optionalText(item.shortProduct),
  };
}

async function fetchJevJSON<T>(
  path: string,
  map: (raw: unknown) => T,
  etag?: string | null,
  signal?: AbortSignal,
): Promise<JevFetchResult<T>> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (etag) headers["If-None-Match"] = etag;
  const response = await fetch(jevUrl(path), {
    method: "GET",
    headers,
    cache: "no-store",
    signal,
  });
  if (response.status === 304) {
    return { status: "unchanged", etag: response.headers.get("ETag") ?? etag ?? null };
  }
  if (!response.ok) {
    throw new Error(`JEV API 请求失败 (${response.status})`);
  }
  return {
    status: "updated",
    data: map(await response.json()),
    etag: response.headers.get("ETag"),
  };
}

function jevPath(path: string, strategyVersion?: string | null): string {
  if (!strategyVersion) return path;
  const sep = path.includes("?") ? "&" : "?";
  return `${path}${sep}strategyVersion=${encodeURIComponent(strategyVersion)}`;
}

export function fetchJevSnapshot(etag?: string | null, signal?: AbortSignal, strategyVersion?: string | null) {
  return fetchJevJSON(jevPath("/snapshot", strategyVersion), mapJevSnapshot, etag, signal);
}

export function fetchJevPositions(etag?: string | null, signal?: AbortSignal, strategyVersion?: string | null) {
  return fetchJevJSON(
    jevPath("/positions?limit=50", strategyVersion),
    (raw) => mapList(raw, mapJevPosition),
    etag,
    signal,
  );
}

export function fetchJevDecisions(etag?: string | null, signal?: AbortSignal, strategyVersion?: string | null) {
  return fetchJevJSON(
    jevPath("/decisions?limit=50", strategyVersion),
    (raw) => mapList(raw, mapJevDecision),
    etag,
    signal,
  );
}

export function fetchJevTrades(etag?: string | null, signal?: AbortSignal, strategyVersion?: string | null) {
  return fetchJevJSON(jevPath("/trades?limit=50", strategyVersion), (raw) => mapList(raw, mapJevTrade), etag, signal);
}

export function fetchJevEquity(rangeValue = "24h", etag?: string | null, signal?: AbortSignal, strategyVersion?: string | null) {
  return fetchJevJSON(
    jevPath(`/equity?range=${encodeURIComponent(rangeValue)}`, strategyVersion),
    mapEquity,
    etag,
    signal,
  );
}

function mapList<T>(raw: unknown, mapItem: (item: unknown, index: number) => T): T[] {
  if (raw === null || typeof raw !== "object") {
    throw new Error("JEV list 无效");
  }
  const payload = raw as Record<string, unknown>;
  const data = payload.data;
  if (!Array.isArray(data)) {
    throw new Error("data 必须是数组");
  }
  return data.map(mapItem);
}

function mapEquity(raw: unknown): JevEquityCurve {
  if (raw === null || typeof raw !== "object") {
    throw new Error("JEV equity 无效");
  }
  const payload = raw as Record<string, unknown>;
  const points = payload.points;
  if (!Array.isArray(points)) {
    throw new Error("points 必须是数组");
  }
  return {
    range: text(payload.range, "range"),
    points: points.map((item, index) => {
      if (item === null || typeof item !== "object") {
        throw new Error(`points[${index}] 无效`);
      }
      const point = item as Record<string, unknown>;
      return {
        ts: text(point.ts, "ts"),
        equity: text(point.equity, "equity"),
        netPnl: text(point.netPnl, "netPnl"),
      };
    }),
  };
}
