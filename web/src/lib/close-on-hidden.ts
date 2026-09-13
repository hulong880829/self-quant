import * as React from "react";

export function useCloseOnHidden(close: () => void) {
  const hostRef = React.useRef<HTMLDivElement>(null);
  const closeRef = React.useRef(close);
  closeRef.current = close;

  React.useLayoutEffect(() => {
    closeRef.current();
  }, []);

  return hostRef;
}
