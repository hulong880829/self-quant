"use client";

import * as React from "react";
import { Activity } from "react";
import { usePathname } from "next/navigation";

import { useAuth } from "@/components/auth/auth-provider";
import {
  isKeepAlivePath,
  workspaceIdentity,
} from "@/components/layout/workspace-owner";

export function PersistentWorkspaceBoundary({
  children,
  fundingPage,
  tradingPage,
}: {
  children: React.ReactNode;
  fundingPage?: React.ReactNode;
  tradingPage?: React.ReactNode;
}) {
  const pathname = usePathname();
  const { status, user } = useAuth();
  const [owner, setOwner] = React.useState<string | null>(null);
  const [ownerEpoch, setOwnerEpoch] = React.useState(0);
  const visitedRef = React.useRef({
    funding: false,
    trading: false,
    epoch: 0,
  });
  const fallbackRef = React.useRef<{
    funding: React.ReactNode | null;
    trading: React.ReactNode | null;
  }>({ funding: null, trading: null });

  const identity = workspaceIdentity(status, user);
  if (identity != null && owner == null) {
    setOwner(identity);
  } else if (identity != null && owner != null && owner !== identity) {
    setOwner(identity);
    setOwnerEpoch((current) => current + 1);
  }

  if (visitedRef.current.epoch !== ownerEpoch) {
    visitedRef.current = { funding: false, trading: false, epoch: ownerEpoch };
    fallbackRef.current = { funding: null, trading: null };
  }

  if (pathname === "/funding") {
    visitedRef.current.funding = true;
    if (fallbackRef.current.funding == null) {
      fallbackRef.current.funding = children;
    }
  }
  if (pathname === "/trading") {
    visitedRef.current.trading = true;
    if (fallbackRef.current.trading == null) {
      fallbackRef.current.trading = children;
    }
  }

  const keepAlive = isKeepAlivePath(pathname);

  return (
    <div data-workspace-epoch={ownerEpoch}>
      {visitedRef.current.funding ? (
        <Activity
          key={`funding:${ownerEpoch}`}
          mode={pathname === "/funding" ? "visible" : "hidden"}
        >
          <div data-workspace-slot="funding">
            {fundingPage ?? fallbackRef.current.funding}
          </div>
        </Activity>
      ) : null}
      {visitedRef.current.trading ? (
        <Activity
          key={`trading:${ownerEpoch}`}
          mode={pathname === "/trading" ? "visible" : "hidden"}
        >
          <div data-workspace-slot="trading">
            {tradingPage ?? fallbackRef.current.trading}
          </div>
        </Activity>
      ) : null}
      {keepAlive ? null : children}
    </div>
  );
}
