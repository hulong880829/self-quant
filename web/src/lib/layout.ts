export const HEADER_HEIGHT_PX = 64;
export const ASSISTANT_DOCK_MIN_WIDTH_PX = 1600;
export const ASSISTANT_DOCK_MIN_HEIGHT_PX = 720;
export const ASSISTANT_OPEN_WIDTH_PX = 350;
export const ASSISTANT_RAIL_WIDTH_PX = 56;
export const ASSISTANT_MAIN_PADDING_OPEN_PX = 366;
export const ASSISTANT_MAIN_PADDING_CLOSED_PX = 72;
export const COMPACT_NAV_MAX_WIDTH_PX = 1279;
export const FUNDING_DETAIL_WIDTH_PX = 380;
export const MIN_READABLE_MAIN_WIDTH_PX = 900;

export type NavDensity = "compact" | "full";
export type AssistantSurface = "dock" | "sheet";

export function navDensity(viewportWidth: number): NavDensity {
  return viewportWidth <= COMPACT_NAV_MAX_WIDTH_PX ? "compact" : "full";
}

export function shouldDockAssistant(
  viewportWidth: number,
  viewportHeight: number,
): boolean {
  return (
    viewportWidth >= ASSISTANT_DOCK_MIN_WIDTH_PX &&
    viewportHeight >= ASSISTANT_DOCK_MIN_HEIGHT_PX
  );
}

export function assistantSurface(
  viewportWidth: number,
  viewportHeight: number,
): AssistantSurface {
  return shouldDockAssistant(viewportWidth, viewportHeight) ? "dock" : "sheet";
}

export function assistantMainPaddingPx(
  docked: boolean,
  assistantOpen: boolean,
): number {
  if (!docked) return 0;
  return assistantOpen
    ? ASSISTANT_MAIN_PADDING_OPEN_PX
    : ASSISTANT_MAIN_PADDING_CLOSED_PX;
}

export function pageContentWidthPx(
  viewportWidth: number,
  viewportHeight: number,
  assistantOpen: boolean,
): number {
  return (
    viewportWidth -
    assistantMainPaddingPx(
      shouldDockAssistant(viewportWidth, viewportHeight),
      assistantOpen,
    )
  );
}

export function shouldUseSplitLayout(
  contentWidth: number,
  sidebarWidth: number,
  minMainWidth = MIN_READABLE_MAIN_WIDTH_PX,
): boolean {
  return contentWidth >= sidebarWidth + minMainWidth;
}

export const pageMinHeightClassName = "min-h-[var(--app-page-min-height)]";
export const panelHeightClassName =
  "h-[var(--app-panel-height)] min-h-0 max-h-[var(--app-panel-height)]";
