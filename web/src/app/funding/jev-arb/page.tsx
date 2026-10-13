import type { Metadata } from "next";

import { JevArbDashboard } from "@/components/jev-arb/dashboard";

export const metadata: Metadata = {
  title: "JEV 纸面套利",
};

export default function JevArbPage() {
  return <JevArbDashboard />;
}
