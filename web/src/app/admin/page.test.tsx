// @vitest-environment jsdom

import * as React from "react";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const auth = vi.hoisted(() => ({
  status: "authenticated" as "authenticated" | "anonymous" | "loading",
  user: { username: "alice", permission: "user" } as {
    username: string;
    permission: string;
  } | null,
  openLogin: vi.fn(),
}));

vi.mock("@/components/auth/auth-provider", () => ({
  useAuth: () => auth,
}));

vi.mock("@/components/admin/admin-dashboard", () => ({
  AdminDashboard: () => <div>admin dashboard</div>,
}));

import { AdminBody } from "./page";

describe("AdminBody", () => {
  afterEach(cleanup);

  it("rejects authenticated users without admin permission", () => {
    auth.user = { username: "alice", permission: "user" };
    render(<AdminBody />);
    expect(screen.getByText("403")).toBeTruthy();
    expect(screen.queryByText("admin dashboard")).toBeNull();
  });

  it("renders the dashboard for administrators", () => {
    auth.user = { username: "root", permission: "admin" };
    render(<AdminBody />);
    expect(screen.getByText("admin dashboard")).toBeTruthy();
  });
});
