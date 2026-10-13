"use client";

import * as React from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { WideTableScroll } from "@/components/layout/responsive";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import {
  createAdminBroker,
  deleteAdminBroker,
  disableAdminBroker,
  resetAdminBrokerSecret,
  restoreAdminBroker,
  type Broker,
} from "@/lib/api/admin";

export function BrokerTable({
  brokers,
  onChanged,
}: {
  brokers: Broker[];
  onChanged: () => Promise<void>;
}) {
  const [name, setName] = React.useState("");
  const [secret, setSecret] = React.useState("");
  const [pending, setPending] = React.useState("");
  const [confirmId, setConfirmId] = React.useState("");
  const [confirmText, setConfirmText] = React.useState("");
  const [error, setError] = React.useState("");

  async function run(id: string, task: () => Promise<Broker | void>) {
    setPending(id);
    setError("");
    try {
      const result = await task();
      if (result && result.clientSecret) {
        setSecret(result.clientSecret);
      }
      await onChanged();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Broker 操作失败");
    } finally {
      setPending("");
    }
  }

  return (
    <div className="flex min-h-0 flex-col">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b px-4 py-3">
        <div>
          <h2 className="text-base font-semibold">Broker</h2>
          <p className="text-xs text-muted-foreground">Client Secret 只在创建或重置时展示一次。</p>
        </div>
        <form
          className="flex items-center gap-2"
          onSubmit={(event) => {
            event.preventDefault();
            void run("create", () => createAdminBroker(name.trim() || "Broker"));
            setName("");
          }}
        >
          <Input value={name} onChange={(event) => setName(event.target.value)} placeholder="Broker 名称" />
          <Button type="submit" disabled={pending === "create"}>
            创建
          </Button>
        </form>
      </div>
      {error ? <div className="px-4 py-2 text-sm text-destructive">{error}</div> : null}
      <WideTableScroll>
        <table className="w-full min-w-[720px] text-left text-sm">
          <thead className="text-xs text-muted-foreground">
            <tr>
              <th className="px-4 py-2">ID</th>
              <th className="px-4 py-2">名称</th>
              <th className="px-4 py-2">状态</th>
              <th className="px-4 py-2">Secret</th>
              <th className="px-4 py-2">操作</th>
            </tr>
          </thead>
          <tbody>
            {brokers.map((broker) => (
              <tr key={broker.brokerId} className="border-t">
                <td className="px-4 py-2 font-mono text-xs">{broker.brokerId}</td>
                <td className="px-4 py-2">{broker.name}</td>
                <td className="px-4 py-2">{broker.status}</td>
                <td className="px-4 py-2 font-mono text-xs">{broker.clientSecretMasked}</td>
                <td className="px-4 py-2">
                  <div className="flex flex-wrap gap-2">
                    {broker.status === "active" ? (
                      <Button size="sm" variant="outline" disabled={!!pending} onClick={() => void run(broker.brokerId, () => disableAdminBroker(broker.brokerId))}>
                        禁用
                      </Button>
                    ) : (
                      <Button size="sm" variant="outline" disabled={!!pending} onClick={() => void run(broker.brokerId, () => restoreAdminBroker(broker.brokerId))}>
                        恢复
                      </Button>
                    )}
                    <Button size="sm" variant="outline" disabled={!!pending} onClick={() => void run(broker.brokerId, () => resetAdminBrokerSecret(broker.brokerId))}>
                      重置 Secret
                    </Button>
                    <Button size="sm" variant="destructive" disabled={!!pending} onClick={() => { setConfirmId(broker.brokerId); setConfirmText(""); }}>
                      删除
                    </Button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </WideTableScroll>
      <Sheet open={!!secret} onOpenChange={(open) => !open && setSecret("")}>
        <SheetContent>
          <SheetHeader>
            <SheetTitle>Client Secret</SheetTitle>
            <SheetDescription>请立即复制保存，关闭后无法再次查看明文。</SheetDescription>
          </SheetHeader>
          <div className="break-all rounded-md border bg-muted px-3 py-2 font-mono text-sm">{secret}</div>
          <SheetFooter>
            <Button onClick={() => void navigator.clipboard.writeText(secret)}>复制</Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>
      <Sheet open={!!confirmId} onOpenChange={(open) => !open && setConfirmId("")}>
        <SheetContent>
          <SheetHeader>
            <SheetTitle>删除 Broker</SheetTitle>
            <SheetDescription>请输入 Broker ID 以确认删除。仍有归属账户时无法删除。</SheetDescription>
          </SheetHeader>
          <Input value={confirmText} onChange={(event) => setConfirmText(event.target.value)} placeholder={confirmId} />
          <SheetFooter>
            <Button
              variant="destructive"
              disabled={confirmText !== confirmId || !!pending}
              onClick={() => {
                const id = confirmId;
                setConfirmId("");
                void run(id, () => deleteAdminBroker(id));
              }}
            >
              确认删除
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>
    </div>
  );
}
