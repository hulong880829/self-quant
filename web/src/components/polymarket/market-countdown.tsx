"use client";

import * as React from "react";

function formatCountdown(totalSeconds: number) {
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60)
    .toString()
    .padStart(2, "0");
  const seconds = (totalSeconds % 60).toString().padStart(2, "0");
  if (hours > 0) {
    return `${hours}:${minutes}:${seconds}`;
  }
  return `${minutes}:${seconds}`;
}

export function MarketCountdown({
  windowEnd,
  onWindowEnd,
}: {
  windowEnd: string | null;
  onWindowEnd: () => void;
}) {
  const [countdownSeconds, setCountdownSeconds] = React.useState(0);
  const previousCountdown = React.useRef<number | null>(null);

  React.useEffect(() => {
    const update = () => {
      setCountdownSeconds(
        windowEnd
          ? Math.max(0, Math.ceil((new Date(windowEnd).getTime() - Date.now()) / 1000))
          : 0,
      );
    };
    update();
    const timer = setInterval(update, 1_000);
    return () => clearInterval(timer);
  }, [windowEnd]);

  React.useEffect(() => {
    if (
      previousCountdown.current != null &&
      previousCountdown.current > 0 &&
      countdownSeconds === 0
    ) {
      onWindowEnd();
    }
    previousCountdown.current = countdownSeconds;
  }, [countdownSeconds, onWindowEnd]);

  return (
    <div className="mt-1 font-mono text-2xl font-semibold text-negative">
      {formatCountdown(countdownSeconds)}
    </div>
  );
}
