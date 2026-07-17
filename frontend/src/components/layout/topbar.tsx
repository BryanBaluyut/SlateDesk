import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { LogOut, Moon, Search, Sun } from "lucide-react";

import { logout } from "@/api/auth";
import type { User } from "@/api/types";
import { useTheme } from "@/components/theme";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";

export function initials(name: string, email: string): string {
  const source = name.trim() || email;
  const parts = source.split(/\s+/).filter(Boolean);
  if (parts.length >= 2) {
    return ((parts[0]?.[0] ?? "") + (parts[1]?.[0] ?? "")).toUpperCase();
  }
  return source.slice(0, 2).toUpperCase();
}

export function Topbar({
  user,
  onOpenPalette,
}: {
  user: User;
  onOpenPalette: () => void;
}) {
  const { theme, toggleTheme } = useTheme();
  const queryClient = useQueryClient();
  const navigate = useNavigate();

  async function onLogout() {
    try {
      await logout();
    } finally {
      // Session is gone (or was already); drop all cached data.
      queryClient.clear();
      void navigate({ to: "/login" });
    }
  }

  return (
    <header className="flex h-13 shrink-0 items-center justify-between gap-4 border-b px-4">
      <button
        type="button"
        onClick={onOpenPalette}
        className="flex h-8 w-full max-w-64 items-center gap-2 rounded-md border bg-muted/40 px-3 text-sm text-muted-foreground transition-colors hover:bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >
        <Search className="size-3.5" aria-hidden="true" />
        <span className="flex-1 text-left">Search…</span>
        <kbd className="pointer-events-none rounded border bg-background px-1.5 font-mono text-[10px] leading-4 text-muted-foreground">
          ⌘K
        </kbd>
      </button>

      <div className="flex items-center gap-1">
        <Button
          variant="ghost"
          size="icon"
          onClick={toggleTheme}
          aria-label={
            theme === "dark" ? "Switch to light mode" : "Switch to dark mode"
          }
        >
          {theme === "dark" ? (
            <Sun className="size-4" aria-hidden="true" />
          ) : (
            <Moon className="size-4" aria-hidden="true" />
          )}
        </Button>

        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button
              variant="ghost"
              size="icon"
              className="rounded-full"
              aria-label="Account menu"
            >
              <Avatar className="size-7">
                <AvatarFallback className="text-[11px]">
                  {initials(user.name, user.email)}
                </AvatarFallback>
              </Avatar>
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="w-56">
            <DropdownMenuLabel className="font-normal">
              <div className="truncate text-sm font-medium">
                {user.name || user.email}
              </div>
              <div className="truncate text-xs text-muted-foreground">
                {user.email}
              </div>
            </DropdownMenuLabel>
            <DropdownMenuSeparator />
            <DropdownMenuItem onSelect={() => void onLogout()}>
              <LogOut className="size-4" aria-hidden="true" />
              Log out
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </header>
  );
}
