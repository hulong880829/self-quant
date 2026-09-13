import * as React from "react";

export function useVisibleMeasure(measure: () => void, enabled: boolean) {
  const measureRef = React.useRef(measure);
  measureRef.current = measure;

  React.useEffect(() => {
    if (!enabled) return;
    const frame = window.requestAnimationFrame(() => {
      measureRef.current();
    });
    return () => window.cancelAnimationFrame(frame);
  }, [enabled]);
}
