import { describe, expect, it } from "vitest";

import {
  isKeepAlivePath,
  workspaceIdentity,
} from "@/components/layout/workspace-owner";

describe("workspaceIdentity", () => {
  it("does not resolve an owner while auth is loading", () => {
    expect(workspaceIdentity("loading", { username: "alice" })).toBeNull();
    expect(workspaceIdentity("loading", null)).toBeNull();
  });

  it("records user and anonymous identities after resolve", () => {
    expect(workspaceIdentity("authenticated", { username: "alice" })).toBe(
      "user:alice",
    );
    expect(workspaceIdentity("anonymous", null)).toBe("anonymous");
  });

  it("only keeps funding and trading paths", () => {
    expect(isKeepAlivePath("/funding")).toBe(true);
    expect(isKeepAlivePath("/trading")).toBe(true);
    expect(isKeepAlivePath("/accounts")).toBe(false);
  });
});
