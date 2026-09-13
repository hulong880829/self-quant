import { describe, expect, it } from "vitest";

import { isAbortError } from "./abort";

describe("isAbortError", () => {
  it("accepts AbortError instances", () => {
    expect(
      isAbortError(new DOMException("signal is aborted without reason", "AbortError")),
    ).toBe(true);
    const named = new Error("Aborted");
    named.name = "AbortError";
    expect(isAbortError(named)).toBe(true);
    expect(
      isAbortError({ name: "AbortError", message: "signal is aborted without reason" }),
    ).toBe(true);
  });

  it("rejects non-abort values", () => {
    const error = new Error("trader service unavailable");
    expect(isAbortError(error)).toBe(false);
    expect(isAbortError("AbortError")).toBe(false);
    expect(isAbortError(null)).toBe(false);
  });
});
