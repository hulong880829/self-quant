import type { Metadata } from "next";

import { FundingDashboard } from "@/components/funding/funding-dashboard";

export const metadata: Metadata = {
  title: "套利机会",
};

export default function FundingPage() {
  return <FundingDashboard />;
}
