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
  createAdminPlatformAccount,
  deleteAdminPlatformAccount,
  disableAdminPlatformAccount,
  resetAdminPlatformAccountPassword,
  restoreAdminPlatformAccount,
  updateAdminPlatformAccountBroker,
  type Broker,
  type PlatformAccount,
} from "@/lib/api/admin";

export function PlatformAccountTable({
  accounts,
  brokers,
  onChanged,
}: {
  accounts: PlatformAccount[];
  brokers: Broker[];
  onChanged: () => Promise<void>;
}) {
  const [username, setUsername] = React.useState("");
  const [password, setPassword] = React.useState("");
  const [brokerId, setBrokerId] = React.useState("");
  const [pending, setPending] = React.useState("");
  const [confirmName, setConfirmName] = React.useState("");
  const [confirmText, setConfirmText] = React.useState("");
  const [resetName, setResetName] = React.useState("");
  const [resetPassword, setResetPassword] = React.useState("");
  const [error, setError] = React.useState("");

  async function run(id: string, task: () => Promise<unknown>) {
    setPending(id);
    setError("");
    try {
      await task();
      await onChanged();
    } catch (err) {
      setError(err instanceof Error ? err.message : "平台账户操作失败");
    } finally {
      setPending("");
    }
  }

  return (
    <div className="flex min-h-0 flex-col">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b px-4 py-3">
        <div>
          <h2 className="text-base font-semibold">平台账户</h2>
          <p className="text-xs text-muted-foreground">删除需二次确认并输入用户名。有活动任务时会拒绝删除。</p>
        </div>
        <form
          className="flex flex-wrap items-center gap-2"
          onSubmit={(event) => {
            event.preventDefault();
            void run("create", () =>
              createAdminPlatformAccount({
                username: username.trim(),
                password,
                brokerId: brokerId.trim() || undefined,
              }),
            );
            setUsername("");
            setPassword("");
          }}
        >
          <Input value={username} onChange={(event) => setUsername(event.target.value)} placeholder="用户名" />
          <Input type="password" value={password} onChange={(event) => setPassword(event.target.value)} placeholder="密码" />
          <Input value={brokerId} onChange={(event) => setBrokerId(event.target.value)} placeholder="Broker ID（可空）" />
          <Button type="submit" disabled={pending === "create" || !username.trim() || !password}>
            创建
          </Button>
        </form>
      </div>
      {error ? <div className="px-4 py-2 text-sm text-destructive">{error}</div> : null}
      <WideTableScroll>
        <table className="w-full min-w-[760px] text-left text-sm">
          <thead className="text-xs text-muted-foreground">
            <tr>
              <th className="px-4 py-2">用户名</th>
              <th className="px-4 py-2">状态</th>
              <th className="px-4 py-2">Broker</th>
              <th className="px-4 py-2">来源</th>
              <th className="px-4 py-2">操作</th>
            </tr>
          </thead>
          <tbody>
            {accounts.map((account) => (
              <tr key={account.username} className="border-t">
                <td className="px-4 py-2">{account.username}</td>
                <td className="px-4 py-2">{account.status}</td>
                <td className="px-4 py-2">
                  <select
                    className="rounded-md border bg-background px-2 py-1 text-xs"
                    value={account.brokerId}
                    disabled={!!pending}
                    onChange={(event) => {
                      const next = event.target.value;
                      void run(account.username, () => updateAdminPlatformAccountBroker(account.username, next));
                    }}
                  >
                    <option value="">Self-Quant</option>
                    {brokers.map((broker) => (
                      <option key={broker.brokerId} value={broker.brokerId}>
                        {broker.name}
                      </option>
                    ))}
                  </select>
                </td>
                <td className="px-4 py-2">{account.createdSource}</td>
                <td className="px-4 py-2">
                  <div className="flex flex-wrap gap-2">
                    {account.status === "active" ? (
                      <Button size="sm" variant="outline" disabled={!!pending} onClick={() => void run(account.username, () => disableAdminPlatformAccount(account.username))}>
                        停用
                      </Button>
                    ) : account.status !== "archived" ? (
                      <Button size="sm" variant="outline" disabled={!!pending} onClick={() => void run(account.username, () => restoreAdminPlatformAccount(account.username))}>
                        恢复
                      </Button>
                    ) : null}
                    <Button size="sm" variant="outline" disabled={!!pending} onClick={() => { setResetName(account.username); setResetPassword(""); }}>
                      重置密码
                    </Button>
                    <Button size="sm" variant="destructive" disabled={!!pending} onClick={() => { setConfirmName(account.username); setConfirmText(""); }}>
                      删除
                    </Button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </WideTableScroll>
      <Sheet open={!!confirmName} onOpenChange={(open) => !open && setConfirmName("")}>
        <SheetContent>
          <SheetHeader>
            <SheetTitle>删除平台账户</SheetTitle>
            <SheetDescription>请输入用户名 {confirmName} 以确认删除。</SheetDescription>
          </SheetHeader>
          <Input value={confirmText} onChange={(event) => setConfirmText(event.target.value)} placeholder={confirmName} />
          <SheetFooter>
            <Button
              variant="destructive"
              disabled={confirmText !== confirmName || !!pending}
              onClick={() => {
                const name = confirmName;
                setConfirmName("");
                void run(name, () => deleteAdminPlatformAccount(name));
              }}
            >
              确认删除
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>
      <Sheet open={!!resetName} onOpenChange={(open) => !open && setResetName("")}>
        <SheetContent>
          <SheetHeader>
            <SheetTitle>重置密码</SheetTitle>
            <SheetDescription>为 {resetName} 设置新密码。</SheetDescription>
          </SheetHeader>
          <Input type="password" value={resetPassword} onChange={(event) => setResetPassword(event.target.value)} placeholder="新密码" />
          <SheetFooter>
            <Button
              disabled={!resetPassword || !!pending}
              onClick={() => {
                const name = resetName;
                const next = resetPassword;
                setResetName("");
                void run(name, () => resetAdminPlatformAccountPassword(name, next));
              }}
            >
              保存
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>
    </div>
  );
}
