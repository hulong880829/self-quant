import { describe, expect, it } from "vitest";

import {
  isAuthRequiredPath,
  parseAuthSession,
} from "./auth";

describe("parseAuthSession", () => {
  it("returns null for anonymous payloads", () => {
    expect(parseAuthSession({ authenticated: false })).toBeNull();
  });

  it("maps authenticated payloads", () => {
    expect(
      parseAuthSession({
        authenticated: true,
        username: "admin",
        permission: "admin",
      }),
    ).toEqual({ username: "admin", permission: "admin" });
  });

  it("rejects authenticated payloads without username", () => {
    expect(parseAuthSession({ authenticated: true })).toBeNull();
  });
});

describe("isAuthRequiredPath", () => {
  it("guards trading, accounts, reports and AI settings routes", () => {
    expect(isAuthRequiredPath("/trading")).toBe(true);
    expect(isAuthRequiredPath("/accounts")).toBe(true);
    expect(isAuthRequiredPath("/accounts/settings")).toBe(true);
    expect(isAuthRequiredPath("/reports")).toBe(true);
    expect(isAuthRequiredPath("/reports/detail")).toBe(true);
    expect(isAuthRequiredPath("/ai-settings")).toBe(true);
    expect(isAuthRequiredPath("/funding")).toBe(false);
    expect(isAuthRequiredPath("/orderbook")).toBe(false);
  });
});
