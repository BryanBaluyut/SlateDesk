import { Link } from "@tanstack/react-router";
import { PanelLeft } from "lucide-react";
import { useState } from "react";

import type { Role } from "@/api/types";
import { navItemsFor } from "@/components/layout/nav";
import { cn } from "@/lib/utils";

const COLLAPSE_KEY = "slatedesk-sidebar-collapsed";

/**
 * Left navigation rail. Collapsible to an icon rail; the choice persists.
 * Every link keeps a visible keyboard-focus ring.
 */
export function Sidebar({ role }: { role: Role }) {
  const [collapsed, setCollapsed] = useState(
    () => localStorage.getItem(COLLAPSE_KEY) === "1",
  );

  function toggleCollapsed() {
    setCollapsed((c) => {
      localStorage.setItem(COLLAPSE_KEY, c ? "0" : "1");
      return !c;
    });
  }

  return (
    <aside
      className={cn(
        "flex shrink-0 flex-col border-r border-sidebar-border bg-sidebar text-sidebar-foreground transition-[width] duration-150",
        collapsed ? "w-13" : "w-56",
      )}
    >
      <div
        className={cn(
          "flex h-13 items-center gap-2 border-b border-sidebar-border",
          collapsed ? "justify-center px-0" : "px-4",
        )}
      >
        <div className="flex size-6 shrink-0 items-center justify-center rounded-md bg-primary text-xs font-semibold text-primary-foreground">
          S
        </div>
        {!collapsed && (
          <span className="truncate text-sm font-semibold tracking-tight">
            SlateDesk
          </span>
        )}
      </div>

      <nav
        aria-label="Main"
        className={cn("flex flex-1 flex-col gap-0.5 py-2", collapsed ? "px-2" : "px-2")}
      >
        {navItemsFor(role).map((item) => (
          <Link
            key={item.to}
            to={item.to}
            title={collapsed ? item.label : undefined}
            className={cn(
              "flex items-center gap-2.5 rounded-md px-2.5 py-1.5 text-sm text-sidebar-foreground/70",
              "hover:bg-sidebar-accent hover:text-sidebar-accent-foreground",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-sidebar-ring",
              collapsed && "justify-center px-0 py-2",
            )}
            activeProps={{
              className: "bg-sidebar-accent font-medium text-sidebar-accent-foreground",
              "aria-current": "page",
            }}
          >
            <item.icon className="size-4 shrink-0" aria-hidden="true" />
            {!collapsed && <span className="truncate">{item.label}</span>}
            {!collapsed && item.soon && (
              <span className="ml-auto rounded-full border px-1.5 text-[10px] leading-4 text-muted-foreground">
                soon
              </span>
            )}
          </Link>
        ))}
      </nav>

      <div className={cn("border-t border-sidebar-border p-2", collapsed && "flex justify-center")}>
        <button
          type="button"
          onClick={toggleCollapsed}
          aria-label={collapsed ? "Expand sidebar" : "Collapse sidebar"}
          className={cn(
            "flex items-center gap-2.5 rounded-md px-2.5 py-1.5 text-sm text-sidebar-foreground/70",
            "hover:bg-sidebar-accent hover:text-sidebar-accent-foreground",
            "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-sidebar-ring",
            collapsed ? "justify-center px-0 py-2" : "w-full",
          )}
        >
          <PanelLeft className="size-4 shrink-0" aria-hidden="true" />
          {!collapsed && <span>Collapse</span>}
        </button>
      </div>
    </aside>
  );
}
