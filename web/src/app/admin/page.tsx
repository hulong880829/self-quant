"use client";

import { AuthGate } from "@/components/auth/auth-gate";
import { useAuth } from "@/components/auth/auth-provider";
import { AdminDashboard } from "@/components/admin/admin-dashboard";

export function AdminForbidden() {
  return (
    <div className="flex min-h-[var(--app-page-min-height)] items-center justify-center">
      <div className="w-full max-w-xl rounded-2xl border bg-card/85 p-8 text-center shadow-sm">
        <h1 className="text-3xl font-semibold tracking-tight">403</h1>
        <p className="mt-3 text-sm text-muted-foreground">
          当前账户没有管理员权限，无法访问管理后台。
        </p>
      </div>
    </div>
  );
}

export function AdminBody() {
  const { user } = useAuth();
  if (user?.permission !== "admin") {
    return <AdminForbidden />;
  }
  return <AdminDashboard />;
}

export default function AdminPage() {
  return (
    <AuthGate
      redirectTo="/admin"
      title="需要登录"
      description="管理后台包含平台账户与 Broker 操作，请先登录后再访问。"
    >
      <AdminBody />
    </AuthGate>
  );
}
