// @vitest-environment jsdom

import * as React from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const navigationMocks = vi.hoisted(() => ({
  prefetch: vi.fn(),
}));

vi.mock("next/navigation", () => ({
  usePathname: () => "/funding",
  useRouter: () => ({ prefetch: navigationMocks.prefetch }),
}));

vi.mock("next/dynamic", () => ({
  default: () => function DynamicPlaceholder() {
    return null;
  },
}));

vi.mock("next/link", () => ({
  default: React.forwardRef<
    HTMLAnchorElement,
    React.AnchorHTMLAttributes<HTMLAnchorElement> & {
      href: string;
      prefetch?: boolean;
    }
  >(function MockLink({ href, prefetch, onClick, ...props }, ref) {
    void prefetch;
    return (
      <a
        ref={ref}
        href={href}
        onClick={(event) => {
          event.preventDefault();
          onClick?.(event);
        }}
        {...props}
      />
    );
  }),
}));

import { NavigationLinks } from "./app-shell";
import { assistantSurface, navDensity } from "@/lib/layout";

describe("NavigationLinks", () => {
  afterEach(() => {
    cleanup();
    navigationMocks.prefetch.mockReset();
  });

  it("marks the destination pending immediately and settles on remount", () => {
    const requireAuth = vi.fn();
    const view = render(
      <NavigationLinks
        key="/funding"
        pathname="/funding"
        status="authenticated"
        requireAuth={requireAuth}
      />,
    );
    const orderbook = screen.getByRole("link", { name: "聚合盘口" });

    fireEvent.mouseEnter(orderbook);
    expect(navigationMocks.prefetch).toHaveBeenCalledWith("/orderbook");

    fireEvent.click(orderbook);
    expect(orderbook.getAttribute("aria-busy")).toBe("true");
    expect(orderbook.className).toContain("bg-primary/10");

    view.rerender(
      <NavigationLinks
        key="/orderbook"
        pathname="/orderbook"
        status="authenticated"
        requireAuth={requireAuth}
      />,
    );
    const settled = screen.getByRole("link", { name: "聚合盘口" });
    expect(settled.getAttribute("aria-current")).toBe("page");
    expect(settled.getAttribute("aria-busy")).toBeNull();
  });

  it("keeps guarded routes inactive and opens login when anonymous", () => {
    const requireAuth = vi.fn();
    render(
      <NavigationLinks
        pathname="/funding"
        status="anonymous"
        requireAuth={requireAuth}
      />,
    );
    const accounts = screen.getByRole("link", { name: "账户管理" });

    fireEvent.click(accounts);

    expect(requireAuth).toHaveBeenCalledWith("/accounts");
    expect(accounts.getAttribute("aria-busy")).toBeNull();
    expect(accounts.className).not.toContain("bg-primary/10");
  });

  it("does not create pending state for the current route", () => {
    render(
      <NavigationLinks
        pathname="/funding"
        status="authenticated"
        requireAuth={vi.fn()}
      />,
    );
    const funding = screen.getByRole("link", { name: "Crypto 资金费" });

    fireEvent.click(funding);

    expect(funding.getAttribute("aria-current")).toBe("page");
    expect(funding.getAttribute("aria-busy")).toBeNull();
  });

  it("classifies compact navigation and assistant surfaces without measuring jsdom", () => {
    expect(navDensity(1093)).toBe("compact");
    expect(navDensity(1280)).toBe("full");
    expect(assistantSurface(1093, 614)).toBe("sheet");
    expect(assistantSurface(1536, 864)).toBe("sheet");
    expect(assistantSurface(1920, 1080)).toBe("dock");
  });
});
