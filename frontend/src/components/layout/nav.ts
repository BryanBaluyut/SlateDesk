import {
  Inbox,
  LayoutDashboard,
  Settings,
  UsersRound,
  Users,
} from "lucide-react";

import type { Role } from "@/api/types";

/** Shared navigation model for the sidebar and the command palette. */
export interface NavItem {
  to: "/tickets" | "/dashboard" | "/users" | "/teams" | "/settings";
  label: string;
  icon: typeof Inbox;
  /** Placeholder destination — the feature lands in a later milestone. */
  soon?: boolean;
  /** Hidden from non-admins; the route itself is gated as well. */
  adminOnly?: boolean;
}

export const NAV_ITEMS: readonly NavItem[] = [
  { to: "/tickets", label: "Tickets", icon: Inbox },
  { to: "/dashboard", label: "Dashboard", icon: LayoutDashboard },
  { to: "/users", label: "Users", icon: Users },
  { to: "/teams", label: "Teams", icon: UsersRound },
  { to: "/settings", label: "Settings", icon: Settings, adminOnly: true },
];

/** Items visible to a given role (admin-only entries are filtered out). */
export function navItemsFor(role: Role | undefined): readonly NavItem[] {
  return NAV_ITEMS.filter((item) => !item.adminOnly || role === "admin");
}
