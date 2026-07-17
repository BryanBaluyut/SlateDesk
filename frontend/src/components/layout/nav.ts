import { Inbox, Settings, UsersRound, Users } from "lucide-react";

/** Shared navigation model for the sidebar and the command palette. */
export interface NavItem {
  to: "/tickets" | "/users" | "/teams" | "/settings";
  label: string;
  icon: typeof Inbox;
  /** Placeholder destination — the feature lands in a later milestone. */
  soon?: boolean;
}

export const NAV_ITEMS: readonly NavItem[] = [
  { to: "/tickets", label: "Tickets", icon: Inbox, soon: true },
  { to: "/users", label: "Users", icon: Users },
  { to: "/teams", label: "Teams", icon: UsersRound },
  { to: "/settings", label: "Settings", icon: Settings, soon: true },
];
