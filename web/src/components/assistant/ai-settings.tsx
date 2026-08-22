"use client";

import * as React from "react";
import {
  CheckCircle2,
  KeyRound,
  LoaderCircle,
  RefreshCw,
  ShieldCheck,
  Trash2,
  XCircle,
} from "lucide-react";

import { useAIStatus } from "@/components/assistant/ai-status-provider";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  deleteAICredential,
  saveAICredential,
  testAICredential,
} from "@/lib/api/ai";

const statusLabel = {
  loading: "检测中",
  anonymous: "未登录",
  unconfigured: "未配置",
  unknown: "待检测",
  valid: "可用",
  invalid: "不可用",
  error: "读取失败",
} as const;

export function AISettings() {
  const { availability, credential, error, refresh } = useAIStatus();
  const [apiKey, setAPIKey] = React.useState("");
  const [busy, setBusy] = React.useState<"save" | "test" | "delete" | null>(
    null,
  );
  const [formError, setFormError] = React.useState("");

  const save = async (event: React.FormEvent) => {
    event.preventDefault();
    const value = apiKey.trim();
    if (!value || busy) return;
    setBusy("save");
    setFormError("");
    try {
      await saveAICredential(value);
      setAPIKey("");
      await testAICredential();
      await refresh();
    } catch (reason) {
      setFormError(reason instanceof Error ? reason.message : "保存 AI Key 失败");
      await refresh();
    } finally {
      setBusy(null);
    }
  };

  const test = async () => {
    if (busy) return;
    setBusy("test");
    setFormError("");
    try {
      const tested = await testAICredential();
      if (tested.status === "invalid") {
        setFormError(tested.lastError || "OpenRouter 拒绝了这个 API Key");
      }
      await refresh();
    } catch (reason) {
      setFormError(reason instanceof Error ? reason.message : "检测 AI Key 失败");
    } finally {
      setBusy(null);
    }
  };

  const remove = async () => {
    if (
      busy ||
      !window.confirm("确定删除 OpenRouter API Key？删除后投研助手将不可用。")
    ) {
      return;
    }
    setBusy("delete");
    setFormError("");
    try {
      await deleteAICredential();
      await refresh();
    } catch (reason) {
      setFormError(reason instanceof Error ? reason.message : "删除 AI Key 失败");
    } finally {
      setBusy(null);
    }
  };

  const valid = availability === "valid";
  const invalid = availability === "invalid";

  return (
    <div className="mx-auto w-full max-w-3xl rounded-xl border bg-card shadow-sm">
      <div className="flex flex-col gap-4 border-b p-5 sm:flex-row sm:items-start sm:justify-between">
        <div className="flex min-w-0 gap-3">
          <span className="flex size-10 shrink-0 items-center justify-center rounded-xl bg-primary/10 text-primary">
            <KeyRound className="size-5" />
          </span>
          <div>
            <h2 className="font-semibold">OpenRouter Free</h2>
            <p className="mt-1 text-sm leading-6 text-muted-foreground">
              用于 AI 投研助手的免费模型路由。第一期暂不提供模型选择。
            </p>
          </div>
        </div>
        <Badge
          variant={invalid || availability === "error" ? "destructive" : "outline"}
          className={valid ? "border-emerald-500/30 text-emerald-600" : undefined}
        >
          {availability === "loading" && (
            <LoaderCircle className="animate-spin" />
          )}
          {valid && <CheckCircle2 />}
          {invalid && <XCircle />}
          {statusLabel[availability]}
        </Badge>
      </div>

      <div className="space-y-5 p-5">
        {credential && (
          <div className="grid gap-3 rounded-lg border bg-muted/20 p-4 text-sm sm:grid-cols-2">
            <div>
              <div className="text-xs text-muted-foreground">已保存密钥</div>
              <div className="mt-1 font-mono">{credential.apiKeyMasked}</div>
            </div>
            <div>
              <div className="text-xs text-muted-foreground">最后检测时间</div>
              <div className="mt-1">
                {credential.lastTestedAt
                  ? new Date(credential.lastTestedAt).toLocaleString("zh-CN")
                  : "尚未检测"}
              </div>
            </div>
          </div>
        )}

        <form onSubmit={save} className="space-y-3">
          <label htmlFor="openrouter-api-key" className="text-sm font-medium">
            {credential ? "轮换 API Key" : "绑定 API Key"}
          </label>
          <div className="flex flex-col gap-2 sm:flex-row">
            <Input
              id="openrouter-api-key"
              type="password"
              autoComplete="off"
              value={apiKey}
              placeholder="sk-or-v1-..."
              onChange={(event) => setAPIKey(event.target.value)}
              disabled={busy !== null}
              className="h-9"
            />
            <Button type="submit" disabled={!apiKey.trim() || busy !== null}>
              {busy === "save" && <LoaderCircle className="animate-spin" />}
              保存并检测
            </Button>
          </div>
          <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
            <ShieldCheck className="size-3.5" />
            密钥使用 AES-256-GCM 加密保存，保存后不会再次回显明文。
          </div>
        </form>

        {(formError || error || credential?.lastError) && (
          <div className="rounded-lg border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive">
            {formError || error || credential?.lastError}
          </div>
        )}

        {credential && (
          <div className="flex flex-wrap gap-2 border-t pt-4">
            <Button
              type="button"
              variant="outline"
              onClick={() => void test()}
              disabled={busy !== null}
            >
              {busy === "test" ? (
                <LoaderCircle className="animate-spin" />
              ) : (
                <RefreshCw />
              )}
              重新检测
            </Button>
            <Button
              type="button"
              variant="destructive"
              onClick={() => void remove()}
              disabled={busy !== null}
            >
              {busy === "delete" ? (
                <LoaderCircle className="animate-spin" />
              ) : (
                <Trash2 />
              )}
              删除密钥
            </Button>
          </div>
        )}
      </div>
    </div>
  );
}
