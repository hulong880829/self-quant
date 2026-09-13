import type { TraderContractType, TraderInstrument } from "@/lib/api/trader";
import type { ProductGroup } from "@/lib/api/accounts";
import type { FundingOpportunity, FundingSpread } from "@/types/market";

export type ArbitragePrefillLeg = {
  exchange: string;
  contract: TraderContractType;
  base: string;
  quote: string;
  exchangeSymbol?: string;
};

export type ArbitragePrefill = {
  mode: "arbitrage";
  legA: ArbitragePrefillLeg;
  legB: ArbitragePrefillLeg;
  prefillRequestId?: string;
};

export type TradingViewMode = "manual" | "algorithm" | "arbitrage";

export function tradingViewFromSearchParams(
  params: { get(name: string): string | null } | null | undefined,
): TradingViewMode {
  return parseArbitragePrefill(params) ? "arbitrage" : "manual";
}

export function exchangeToSlug(exchange: string): string {
  return exchange.trim().toLowerCase();
}

function appendLeg(
  params: URLSearchParams,
  side: "A" | "B",
  leg: ArbitragePrefillLeg,
) {
  params.set(`leg${side}Exchange`, leg.exchange);
  params.set(`leg${side}Contract`, leg.contract);
  params.set(`leg${side}Base`, leg.base);
  params.set(`leg${side}Quote`, leg.quote);
  if (leg.exchangeSymbol) {
    params.set(`leg${side}ExchangeSymbol`, leg.exchangeSymbol);
  }
}

export function buildArbitragePrefillUrl(
  prefill: Omit<ArbitragePrefill, "mode">,
): string {
  const params = new URLSearchParams();
  params.set("mode", "arbitrage");
  appendLeg(params, "A", prefill.legA);
  appendLeg(params, "B", prefill.legB);
  return `/trading?${params.toString()}`;
}

export function buildArbitragePrefillUrlFromOpportunity(
  opportunity: FundingOpportunity,
): string {
  const slug = exchangeToSlug(opportunity.exchange);
  return buildArbitragePrefillUrl({
    legA: {
      exchange: slug,
      contract: "spot",
      base: opportunity.baseAsset,
      quote: opportunity.quoteAsset,
    },
    legB: {
      exchange: slug,
      contract: "perpetual",
      base: opportunity.baseAsset,
      quote: opportunity.quoteAsset,
      exchangeSymbol: opportunity.exchangeSymbol,
    },
  });
}

export function buildArbitragePrefillUrlFromSpread(
  spread: Pick<
    FundingSpread,
    "baseAsset" | "quoteAsset" | "longLeg" | "shortLeg"
  >,
): string {
  return buildArbitragePrefillUrl({
    legA: {
      exchange: exchangeToSlug(spread.longLeg.exchange),
      contract: "perpetual",
      base: spread.longLeg.baseAsset ?? spread.baseAsset,
      quote: spread.longLeg.quoteAsset ?? spread.quoteAsset,
      exchangeSymbol: spread.longLeg.exchangeSymbol,
    },
    legB: {
      exchange: exchangeToSlug(spread.shortLeg.exchange),
      contract: "perpetual",
      base: spread.shortLeg.baseAsset ?? spread.baseAsset,
      quote: spread.shortLeg.quoteAsset ?? spread.quoteAsset,
      exchangeSymbol: spread.shortLeg.exchangeSymbol,
    },
  });
}

function parseContract(value: string | null): TraderContractType | null {
  if (value === "spot" || value === "perpetual") return value;
  return null;
}

function parseLeg(
  params: { get(name: string): string | null },
  side: "A" | "B",
): ArbitragePrefillLeg | null {
  const exchange = params.get(`leg${side}Exchange`)?.trim().toLowerCase() ?? "";
  const contract = parseContract(params.get(`leg${side}Contract`));
  const base = params.get(`leg${side}Base`)?.trim() ?? "";
  const quote = params.get(`leg${side}Quote`)?.trim() ?? "";
  const exchangeSymbol = params.get(`leg${side}ExchangeSymbol`)?.trim() || undefined;
  if (!exchange || !contract || !base || !quote) return null;
  return { exchange, contract, base, quote, exchangeSymbol };
}

export function parseArbitragePrefill(
  params: { get(name: string): string | null } | null | undefined,
): ArbitragePrefill | null {
  if (!params || params.get("mode") !== "arbitrage") return null;
  const legA = parseLeg(params, "A");
  const legB = parseLeg(params, "B");
  if (!legA || !legB) return null;
  const prefillRequestId = params.get("prefillRequestId")?.trim() || undefined;
  return {
    mode: "arbitrage",
    legA,
    legB,
    ...(prefillRequestId ? { prefillRequestId } : {}),
  };
}

export function arbitragePrefillSignature(prefill: ArbitragePrefill | null): string {
  if (!prefill) return "";
  const leg = (value: ArbitragePrefillLeg) =>
    [
      value.exchange,
      value.contract,
      value.base,
      value.quote,
      value.exchangeSymbol ?? "",
    ].join("|");
  return `${leg(prefill.legA)}::${leg(prefill.legB)}::${prefill.prefillRequestId ?? ""}`;
}

export function findPrefillProductGroup(
  groups: ProductGroup[],
  prefill: ArbitragePrefill,
): ProductGroup | undefined {
  return groups.find((group) => {
    const exchanges = new Set(group.accounts.map((account) => account.exchangeSlug));
    return exchanges.has(prefill.legA.exchange) && exchanges.has(prefill.legB.exchange);
  });
}

export function compactExchangeSymbol(value: string): string {
  return value
    .trim()
    .toUpperCase()
    .replace(/[^\p{L}\p{N}]/gu, "")
    .replace(/SWAP$/, "");
}

export function findPrefillInstrument(
  instruments: TraderInstrument[],
  hint: ArbitragePrefillLeg,
): TraderInstrument | undefined {
  const slug = hint.exchange.toLowerCase();
  const filtered = instruments.filter(
    (item) =>
      item.exchange.toLowerCase() === slug && item.contractType === hint.contract,
  );
  if (hint.exchangeSymbol) {
    const symbol = compactExchangeSymbol(hint.exchangeSymbol);
    const exact = filtered.find(
      (item) => compactExchangeSymbol(item.exchangeSymbol) === symbol,
    );
    if (exact) return exact;
  }
  return filtered.find(
    (item) => item.baseAsset === hint.base && item.quoteAsset === hint.quote,
  );
}
