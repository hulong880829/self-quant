import type { Metadata } from "next";

import { OrderbookDashboard } from "@/components/orderbook/orderbook-dashboard";

export const metadata: Metadata = {
  title: "聚合盘口",
  description: "跨交易所聚合盘口与 Spread BPS 趋势",
};

export default function OrderbookPage() {
  return <OrderbookDashboard />;
}
