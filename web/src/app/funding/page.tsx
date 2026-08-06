import type { Metadata } from "next";

import { FundingDashboard } from "@/components/funding/funding-dashboard";

export const metadata: Metadata = {
  title: "Crypto 资金费",
};

export default function FundingPage() {
  return <FundingDashboard />;
}
