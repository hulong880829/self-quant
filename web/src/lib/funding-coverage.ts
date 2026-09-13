import type { ContractKind } from "@/types/market";

export function historyWindowReady(complete: boolean | undefined): boolean {
  return complete !== false;
}

export function formatHistoryWindow(
  complete: boolean | undefined,
  value: number,
  format: (value: number) => string,
): string {
  if (complete === false) {
    return "历史不足";
  }
  return format(value);
}

const perpetualOnlyExchanges = new Set(["Hyperliquid", "Aster", "Lighter"]);
const fundingDisplayOnlyExchanges = new Set(["Entropy"]);

export function contractKindFromVenue(
  venueContractType?: string,
  exchangeSymbol?: string,
): ContractKind {
  if (venueContractType === "TRADIFI_PERPETUAL") {
    return "tradifi";
  }
  if (venueContractType === "HIP3" || (exchangeSymbol?.includes(":") ?? false)) {
    return "hip3";
  }
  return "crypto";
}

export function bboCanonicalSymbol(leg: {
  exchange: string;
  venueContractType?: string;
  exchangeSymbol: string;
  quoteAsset: string;
  globalSymbol: string;
}): string {
  const hip3 =
    leg.venueContractType === "HIP3" ||
    (leg.exchangeSymbol?.includes(":") ?? false);
  if (leg.exchange.toLowerCase() === "hyperliquid" && hip3) {
    return (
      leg.exchangeSymbol.replace(/[-_/:]/g, "").toUpperCase() +
      leg.quoteAsset.toUpperCase()
    );
  }
  return leg.globalSymbol;
}

export function canStartFundingTrade(
  exchange: string,
  mode: "basis" | "cross" = "basis",
  _extras?: { venueContractType?: string; exchangeSymbol?: string },
): boolean {
  if (fundingDisplayOnlyExchanges.has(exchange)) {
    return false;
  }
  return mode === "cross" || !perpetualOnlyExchanges.has(exchange);
}
