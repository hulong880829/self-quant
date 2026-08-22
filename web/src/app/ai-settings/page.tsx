"use client";

import { Sparkles } from "lucide-react";

import { AISettings } from "@/components/assistant/ai-settings";
import { AuthGate } from "@/components/auth/auth-gate";
import { PageFrame } from "@/components/layout/responsive";

export default function AISettingsPage() {
  return (
    <AuthGate
      redirectTo="/ai-settings"
      title="需要登录"
      description="AI Key 是用户级私密配置，请先登录后再访问。"
    >
      <PageFrame>
        <div>
          <div className="flex items-center gap-1.5 text-[11px] font-semibold tracking-[0.22em] text-primary">
            <Sparkles className="size-3.5" />
            AI CONFIGURATION
          </div>
          <h1 className="mt-1 text-2xl font-semibold tracking-tight">AI 设置</h1>
          <p className="mt-1 max-w-2xl text-sm leading-6 text-muted-foreground">
            管理投研助手使用的模型 API Key。当前仅支持 OpenRouter Free。
          </p>
        </div>
        <AISettings />
      </PageFrame>
    </AuthGate>
  );
}
