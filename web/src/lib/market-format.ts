const compactNumberFormatters = new Map<number, Intl.NumberFormat>();
const currencyFormatters = new Map<string, Intl.NumberFormat>();
const dateTimeFormatter = new Intl.DateTimeFormat("zh-CN", {
  month: "2-digit",
  day: "2-digit",
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hour12: false,
});

export function formatCompactNumber(value: number, digits = 1) {
  let formatter = compactNumberFormatters.get(digits);
  if (!formatter) {
    formatter = new Intl.NumberFormat("zh-CN", {
      notation: "compact",
      maximumFractionDigits: digits,
    });
    compactNumberFormatters.set(digits, formatter);
  }
  return formatter.format(value);
}

export function formatCurrency(value: number, compact = false) {
  const digits = value >= 1000 ? 2 : value >= 1 ? 4 : 6;
  const key = `${compact}:${digits}`;
  let formatter = currencyFormatters.get(key);
  if (!formatter) {
    formatter = new Intl.NumberFormat("en-US", {
      style: "currency",
      currency: "USD",
      notation: compact ? "compact" : "standard",
      minimumFractionDigits: digits,
      maximumFractionDigits: digits,
    });
    currencyFormatters.set(key, formatter);
  }
  return formatter.format(value);
}

export function formatPercent(value: number, digits = 2) {
  const sign = value > 0 ? "+" : "";
  return `${sign}${value.toFixed(digits)}%`;
}

export function formatFundingRate(value: number | null) {
  if (value === null) return "无";
  const sign = value > 0 ? "+" : "";
  return `${sign}${value.toFixed(4)}%`;
}

export function resolveFundingRate(
  nextFundingRate: number | null,
  currentFundingRate: number | null,
) {
  return nextFundingRate ?? currentFundingRate;
}

export function annualize24h(cumulative24h: number) {
  return cumulative24h * 365;
}

export function annualize7d(cumulative7d: number) {
  return (cumulative7d * 365) / 7;
}

export function rateColor(value: number | null) {
  if (value === null) return "text-muted-foreground";
  if (value > 0) return "text-positive";
  if (value < 0) return "text-negative";
  return "text-muted-foreground";
}

export function formatDateTime(value: string) {
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return "—";
  return dateTimeFormatter.format(date);
}

export function formatSettlementCountdown(value: string, now: number) {
  const remaining = Date.parse(value) - now;
  if (!Number.isFinite(remaining)) return "—";
  if (remaining <= 0) return "待结算";

  const totalMinutes = Math.floor(remaining / 60_000);
  const days = Math.floor(totalMinutes / 1_440);
  const hours = Math.floor((totalMinutes % 1_440) / 60);
  const minutes = totalMinutes % 60;

  return days > 0 ? `${days}d ${hours}h ${minutes}m` : `${hours}h ${minutes}m`;
}
