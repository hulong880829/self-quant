import {
  BarChart3,
  BookOpen,
  Construction,
} from "lucide-react";
import { notFound, redirect } from "next/navigation";

const sections = {
  orderbook: {
    title: "聚合盘口",
    eyebrow: "ORDERBOOK AGGREGATOR",
    description: "跨交易所聚合深度、最优报价和流动性分布将在这里呈现。",
    icon: BookOpen,
  },
  reports: {
    title: "报表分析",
    eyebrow: "REPORTS & ANALYTICS",
    description: "收益、资金费、回撤和交易归因分析将在这里呈现。",
    icon: BarChart3,
  },
} as const;

type SectionKey = keyof typeof sections;

export default async function SectionPage({
  params,
}: {
  params: Promise<{ section: string }>;
}) {
  const { section } = await params;
  if (section === "a-shares" || section === "domestic-futures") {
    redirect("/global-stocks");
  }
  if (!(section in sections)) notFound();

  const content = sections[section as SectionKey];
  const Icon = content.icon;

  return (
    <div className="flex min-h-[var(--app-page-min-height)] items-center justify-center">
      <div className="w-full max-w-xl rounded-2xl border bg-card/85 p-8 text-center shadow-sm backdrop-blur sm:p-12">
        <div className="mx-auto flex size-14 items-center justify-center rounded-2xl bg-primary/10 text-primary">
          <Icon className="size-6" />
        </div>
        <div className="mt-6 text-[11px] font-semibold tracking-[0.22em] text-primary">
          {content.eyebrow}
        </div>
        <h1 className="mt-2 text-3xl font-semibold tracking-tight">{content.title}</h1>
        <p className="mx-auto mt-3 max-w-md text-sm leading-6 text-muted-foreground">
          {content.description}
        </p>
        <div className="mt-8 inline-flex items-center gap-2 rounded-full border bg-muted/60 px-4 py-2 text-xs text-muted-foreground">
          <Construction className="size-3.5" />
          静态页面占位 · 后续迭代
        </div>
      </div>
    </div>
  );
}
