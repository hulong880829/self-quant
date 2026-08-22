import type { Metadata } from "next";

import { PolymarketDashboard } from "@/components/polymarket/polymarket-dashboard";
import { PolymarketProvider } from "@/components/polymarket/polymarket-provider";

export const metadata: Metadata = {
  title: "Polymarket",
  description: "Polymarket 实时行情、账户持仓与交易",
};

export default function CryptoOptionsPage() {
  return (
    <PolymarketProvider>
      <PolymarketDashboard />
    </PolymarketProvider>
  );
}
