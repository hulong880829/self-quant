"use client";

import * as React from "react";

import { useAuth } from "@/components/auth/auth-provider";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";

export function LoginDialog() {
  const { loginOpen, closeLogin, login } = useAuth();
  const [username, setUsername] = React.useState("");
  const [password, setPassword] = React.useState("");
  const [error, setError] = React.useState<string | null>(null);
  const [submitting, setSubmitting] = React.useState(false);

  function resetForm() {
    setUsername("");
    setPassword("");
    setError(null);
    setSubmitting(false);
  }

  async function onSubmit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(null);
    setSubmitting(true);
    try {
      await login({ username, password });
      resetForm();
    } catch (err) {
      setError(err instanceof Error ? err.message : "登录失败，请稍后重试");
      setSubmitting(false);
    }
  }

  return (
    <Sheet
      open={loginOpen}
      onOpenChange={(open) => {
        if (!open) {
          resetForm();
          closeLogin();
        }
      }}
    >
      <SheetContent side="right" className="w-full sm:max-w-md">
        <SheetHeader>
          <SheetTitle>登录账户</SheetTitle>
          <SheetDescription>
            登录后可访问实盘交易与账户管理。会话通过安全 Cookie 保持。
          </SheetDescription>
        </SheetHeader>
        <form onSubmit={onSubmit} className="flex flex-1 flex-col gap-4 px-4">
          <label className="grid gap-1.5 text-sm">
            <span className="text-muted-foreground">账户名</span>
            <Input
              autoComplete="username"
              value={username}
              onChange={(event) => setUsername(event.target.value)}
              placeholder="请输入账户名"
              disabled={submitting}
              required
            />
          </label>
          <label className="grid gap-1.5 text-sm">
            <span className="text-muted-foreground">密码</span>
            <Input
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(event) => setPassword(event.target.value)}
              placeholder="请输入密码"
              disabled={submitting}
              required
            />
          </label>
          {error ? (
            <p className="rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm text-destructive">
              {error}
            </p>
          ) : null}
          <SheetFooter className="px-0">
            <Button type="submit" disabled={submitting} className="w-full">
              {submitting ? "登录中…" : "登录"}
            </Button>
          </SheetFooter>
        </form>
      </SheetContent>
    </Sheet>
  );
}
