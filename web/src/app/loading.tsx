export default function AppLoading() {
  return (
    <div
      aria-label="页面加载中"
      className="grid min-h-[var(--app-page-min-height)] min-w-0 gap-3 lg:grid-cols-[14rem_minmax(0,1fr)]"
    >
      <div className="rounded-xl border bg-card/70 p-3 shadow-sm">
        <div className="h-5 w-24 animate-pulse rounded bg-muted" />
        <div className="mt-4 space-y-2">
          {Array.from({ length: 6 }, (_, index) => (
            <div
              key={index}
              className="h-10 animate-pulse rounded-lg bg-muted/70"
            />
          ))}
        </div>
      </div>
      <div className="space-y-3">
        <div className="grid gap-3 sm:grid-cols-3">
          {Array.from({ length: 3 }, (_, index) => (
            <div
              key={index}
              className="h-24 animate-pulse rounded-xl border bg-card/70"
            />
          ))}
        </div>
        <div className="h-[28rem] animate-pulse rounded-xl border bg-card/70 shadow-sm" />
      </div>
    </div>
  );
}
