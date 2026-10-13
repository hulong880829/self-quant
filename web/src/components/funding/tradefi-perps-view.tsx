"use client";

import * as React from "react";
import { type SortingState, flexRender } from "@tanstack/react-table";
import {
  getCoreRowModel,
  getSortedRowModel,
  type LegacyColumnDef,
  useLegacyTable,
} from "@tanstack/react-table/legacy";

import { BasisSpreadPanel } from "@/components/funding/basis-spread-chart";
import { fundingMetricColumns } from "@/components/funding/funding-metric-columns";
import { SortHeader } from "@/components/funding/funding-sort-header";
import { WorkspacePanel } from "@/components/layout/responsive";
import { bboCanonicalSymbol } from "@/lib/funding-coverage";
import { cn } from "@/lib/utils";
import type { FundingFilters, TradefiPerpRow } from "@/types/market";

export function filterTradefiRows(rows: TradefiPerpRow[], filters: FundingFilters) {
  const query = filters.search.trim().toLowerCase();
  return rows.filter((item) => {
    if (
      query &&
      !item.stockSymbol.toLowerCase().includes(query) &&
      !item.exchange.toLowerCase().includes(query) &&
      !item.exchangeSymbol.toLowerCase().includes(query)
    ) {
      return false;
    }
    if (item.positionNotional < filters.minPositionNotional) return false;
    if (item.turnover24hUsd < filters.minDailyVolume) return false;
    if (
      filters.intervalHours !== "all" &&
      item.settlementIntervalHours !== filters.intervalHours
    ) {
      return false;
    }
    if (filters.exchanges.length > 0 && !filters.exchanges.includes(item.exchange)) {
      return false;
    }
    if (filters.contractKinds.length > 0 && !filters.contractKinds.includes("tradifi")) {
      return false;
    }
    if (filters.direction === "positive" && item.fundingRate <= 0) return false;
    if (filters.direction === "negative" && item.fundingRate >= 0) return false;
    return true;
  });
}

export function TradefiPerpsView({
  data,
  loading,
  error = null,
  hasSnapshot = false,
}: {
  data: TradefiPerpRow[];
  loading: boolean;
  error?: string | null;
  hasSnapshot?: boolean;
}) {
  const [sorting, setSorting] = React.useState<SortingState>([
    { id: "annualizedRate", desc: true },
  ]);
  const [selectedRowId, setSelectedRowId] = React.useState<string | null>(null);
  const columns = React.useMemo<LegacyColumnDef<TradefiPerpRow>[]>(
    () => [
      {
        accessorKey: "stockSymbol",
        header: "标的",
        cell: ({ row }) => (
          <div>
            <div className="font-medium">IBKR · {row.original.stockSymbol} 现货</div>
            <div className="text-[11px] text-muted-foreground">
              {row.original.primaryExchange || "—"} · conId {row.original.conId}
            </div>
          </div>
        ),
      },
      {
        accessorKey: "exchange",
        header: "永续",
        cell: ({ row }) => (
          <div>
            <div className="font-medium">
              {row.original.exchange} · {row.original.exchangeSymbol}
            </div>
            <div className="text-[11px] text-muted-foreground">
              {row.original.baseAsset}/{row.original.quoteAsset}
            </div>
          </div>
        ),
      },
      ...fundingMetricColumns<TradefiPerpRow>(),
    ],
    [],
  );
  const table = useLegacyTable({
    data,
    columns,
    state: { sorting },
    onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
  });

  return (
    <WorkspacePanel>
      {error ? (
        <div role="alert" className="border-b px-3 py-2 text-xs text-destructive">
          {error}
        </div>
      ) : null}
      <div className="min-w-0 overflow-auto">
        <table className="w-full min-w-[960px] border-collapse text-sm">
          <thead className="sticky top-0 z-10 bg-muted/80 backdrop-blur">
            {table.getHeaderGroups().map((headerGroup) => (
              <tr key={headerGroup.id} className="border-b">
                {headerGroup.headers.map((header, index) => (
                  <th
                    key={header.id}
                    className={cn("h-11 px-3 text-left font-medium", index >= 2 && "text-right")}
                  >
                    {header.isPlaceholder ? null : header.column.getCanSort() ? (
                      <SortHeader
                        label={String(header.column.columnDef.header)}
                        sorted={header.column.getIsSorted()}
                        align={index >= 2 ? "right" : "left"}
                        onClick={() => header.column.toggleSorting()}
                      />
                    ) : (
                      flexRender(header.column.columnDef.header, header.getContext())
                    )}
                  </th>
                ))}
              </tr>
            ))}
          </thead>
          <tbody>
            {table.getRowModel().rows.length > 0 ? (
              table.getRowModel().rows.map((row) => {
                const id = `${row.original.conId}|${row.original.exchange}|${row.original.exchangeSymbol}`;
                const selected = selectedRowId === id;
                return (
                  <React.Fragment key={id}>
                    <tr
                      className={cn("cursor-pointer border-b", selected && "bg-muted/30")}
                      onClick={() => setSelectedRowId(selected ? null : id)}
                    >
                      {row.getVisibleCells().map((cell, index) => (
                        <td
                          key={cell.id}
                          className={cn("px-3 py-3 align-middle", index >= 2 && "text-right")}
                        >
                          {flexRender(cell.column.columnDef.cell, cell.getContext())}
                        </td>
                      ))}
                    </tr>
                    {selected ? (
                      <tr className="border-b bg-muted/20">
                        <td colSpan={columns.length} className="px-4 py-3">
                          <BasisSpreadPanel
                            venue={row.original.exchange}
                            compareVenue="ibkr"
                            baseAsset={row.original.baseAsset}
                            quoteAsset={row.original.quoteAsset}
                            venueSymbol={bboCanonicalSymbol({
                              exchange: row.original.exchange,
                              exchangeSymbol: row.original.exchangeSymbol,
                              quoteAsset: row.original.quoteAsset,
                              globalSymbol: `${row.original.baseAsset}${row.original.quoteAsset}`,
                            })}
                            compareVenueSymbol={row.original.stockSymbol}
                            identity={{
                              identityMode: "venue_symbol",
                              venueProduct: "perpetual",
                              compareVenueProduct: "equity",
                              venueExchangeSymbol: row.original.exchangeSymbol,
                              compareExchangeSymbol: row.original.conId,
                              priceMultiplier: row.original.priceMultiplier || "1",
                            }}
                          />
                        </td>
                      </tr>
                    ) : null}
                  </React.Fragment>
                );
              })
            ) : (
              <tr>
                <td colSpan={columns.length} className="h-40 text-center text-muted-foreground">
                  {loading
                    ? "正在加载 TradeFi-Perps"
                    : !hasSnapshot && error
                      ? error
                      : "没有符合条件的配对"}
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </WorkspacePanel>
  );
}
