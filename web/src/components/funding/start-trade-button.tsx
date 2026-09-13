"use client";

import { useRouter } from "next/navigation";

import { useAuth } from "@/components/auth/auth-provider";
import { Button } from "@/components/ui/button";
import { createIdempotencyKey } from "@/lib/idempotency-key";

function withPrefillRequestId(href: string): string {
  const url = new URL(href, "https://selfquant.invalid");
  url.searchParams.set("prefillRequestId", createIdempotencyKey());
  return `${url.pathname}${url.search}`;
}

export function StartTradeButton({ href }: { href: string }) {
  const auth = useAuth();
  const router = useRouter();

  return (
    <Button
      type="button"
      size="sm"
      onClick={(event) => {
        event.stopPropagation();
        const nextHref = withPrefillRequestId(href);
        if (!auth.requireAuth(nextHref)) return;
        router.push(nextHref);
      }}
    >
      开启交易
    </Button>
  );
}
