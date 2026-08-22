"use client";

import * as React from "react";
import { LockKeyhole } from "lucide-react";

import { useAuth } from "@/components/auth/auth-provider";
import { Button } from "@/components/ui/button";

export function AuthGate({
  redirectTo,
  title,
  description,
  children,
}: {
  redirectTo: string;
  title: string;
  description: string;
  children: React.ReactNode;
}) {
  const { status, openLogin } = useAuth();

  if (status === "loading") {
    return (
      <div className="flex min-h-[var(--app-page-min-height)] items-center justify-center">
        <div className="rounded-2xl border bg-card/85 px-6 py-4 text-sm text-muted-foreground">
          正在确认登录状态…
        </div>
      </div>
    );
  }

  if (status !== "authenticated") {
    return (
      <div className="flex min-h-[var(--app-page-min-height)] items-center justify-center">
        <div className="w-full max-w-xl rounded-2xl border bg-card/85 p-8 text-center shadow-sm backdrop-blur sm:p-12">
          <div className="mx-auto flex size-14 items-center justify-center rounded-2xl bg-primary/10 text-primary">
            <LockKeyhole className="size-6" />
          </div>
          <h1 className="mt-6 text-3xl font-semibold tracking-tight">{title}</h1>
          <p className="mx-auto mt-3 max-w-md text-sm leading-6 text-muted-foreground">
            {description}
          </p>
          <Button className="mt-8" onClick={() => openLogin(redirectTo)}>
            登录后继续
          </Button>
        </div>
      </div>
    );
  }

  return <>{children}</>;
}
