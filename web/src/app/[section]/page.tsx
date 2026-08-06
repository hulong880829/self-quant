import {
  BarChart3,
  BookOpen,
  CircleDollarSign,
  Construction,
  ShieldCheck,
  TrendingUp,
  WalletCards,
} from "lucide-react";
import { notFound } from "next/navigation";

const sections = {
  "a-shares": {
    title: "国内大A股",
    eyebrow: "CHINA A-SHARES",
    description: "沪深北市场行情、行业轮动、因子研究和组合分析将在这里呈现。",
    icon: BarChart3,
  },
  "domestic-futures": {
    title: "国内期货",
    eyebrow: "CHINA FUTURES",
    description: "国内商品及金融期货行情、期限结构和基差分析将在这里呈现。",
    icon: TrendingUp,
  },
  "crypto-options": {
    title: "Crypto 期权",
    eyebrow: "CRYPTO OPTIONS",
    description: "期权链、隐含波动率、Greeks 和波动率曲面将在这里呈现。",
    icon: CircleDollarSign,
  },
  orderbook: {
    title: "聚合盘口",
    eyebrow: "ORDERBOOK AGGREGATOR",
    description: "跨交易所聚合深度、最优报价和流动性分布将在这里呈现。",
    icon: BookOpen,
  },
  trading: {
    title: "实盘交易",
    eyebrow: "LIVE EXECUTION",
    description: "订单录入、执行状态、风险确认和 OMS 事件将在这里呈现。",
    icon: ShieldCheck,
  },
  accounts: {
    title: "账户管理",
    eyebrow: "ACCOUNT CENTER",
    description: "交易所账户、API 权限、资产和持仓总览将在这里呈现。",
    icon: WalletCards,
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
  if (!(section in sections)) notFound();

  const content = sections[section as SectionKey];
  const Icon = content.icon;

  return (
    <div className="flex min-h-[calc(100vh-7rem)] items-center justify-center">
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
