"use client";

import { AuthGate } from "@/components/auth/auth-gate";
import { AccountDashboard } from "@/components/accounts/account-dashboard";

export default function AccountsPage() {
  return (
    <AuthGate
      redirectTo="/accounts"
      title="需要登录"
      description="账户管理包含 API 权限与资产信息，请先登录后再访问。"
    >
      <AccountDashboard />
    </AuthGate>
  );
}
