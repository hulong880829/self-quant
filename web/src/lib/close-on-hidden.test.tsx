// @vitest-environment jsdom

import * as React from "react";
import { Activity } from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { useCloseOnHidden } from "./close-on-hidden";

function Probe() {
  const [open, setOpen] = React.useState(false);
  const hostRef = useCloseOnHidden(() => setOpen(false));
  return (
    <div ref={hostRef}>
      {open ? <button type="button">确认下单</button> : null}
      <button type="button" onClick={() => setOpen(true)}>
        打开确认
      </button>
      <input aria-label="数量" defaultValue="1" />
    </div>
  );
}

function Harness({ hidden }: { hidden: boolean }) {
  return (
    <Activity mode={hidden ? "hidden" : "visible"}>
      <Probe />
    </Activity>
  );
}

describe("useCloseOnHidden", () => {
  afterEach(cleanup);

  it("hides overlay with the Activity host and closes it when shown again", async () => {
    const view = render(<Harness hidden={false} />);
    fireEvent.click(screen.getByRole("button", { name: "打开确认" }));
    expect(screen.getByRole("button", { name: "确认下单" })).toBeTruthy();

    view.rerender(<Harness hidden />);
    expect(screen.queryByRole("button", { name: "确认下单" })).toBeNull();
    expect((screen.getByLabelText("数量") as HTMLInputElement).value).toBe(
      "1",
    );

    view.rerender(<Harness hidden={false} />);
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "确认下单", hidden: true })).toBeNull(),
    );
    expect((screen.getByLabelText("数量") as HTMLInputElement).value).toBe("1");
  });
});
