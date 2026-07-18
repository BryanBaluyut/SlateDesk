import { createFileRoute, Outlet, redirect } from "@tanstack/react-router";
import { useCallback, useEffect, useState } from "react";

import { meQueryOptions } from "@/api/auth";
import { isApiError } from "@/api/client";
import { CommandPalette } from "@/components/layout/command-palette";
import { Sidebar } from "@/components/layout/sidebar";
import { Topbar } from "@/components/layout/topbar";
import { ShortcutsHelpDialog } from "@/components/tickets/shortcuts-help";
import { useRealtimeEvents } from "@/lib/realtime";
import { useShortcut, useShortcutListener } from "@/lib/shortcuts";

/**
 * Pathless layout guarding everything behind authentication. The session
 * cookie is HttpOnly, so the only way to know whether we are logged in is to
 * ask the API; /auth/me doubles as the current-user fetch the shell needs.
 */
export const Route = createFileRoute("/_auth")({
  beforeLoad: async ({ context, location }) => {
    try {
      const user = await context.queryClient.ensureQueryData(meQueryOptions);
      return { user };
    } catch (err) {
      if (isApiError(err) && err.status === 401) {
        throw redirect({
          to: "/login",
          search: { redirect: location.href },
        });
      }
      throw err;
    }
  },
  component: AuthLayout,
});

function AuthLayout() {
  const { user } = Route.useRouteContext();
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [helpOpen, setHelpOpen] = useState(false);

  // Global command palette shortcut: Cmd+K / Ctrl+K.
  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === "k" && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        setPaletteOpen((open) => !open);
      }
    }
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, []);

  // Workspace keyboard map (j/k/o, r/n, a/s/p, x, /, ?) — one listener,
  // components subscribe to actions. "? " and the help dialog live here.
  useShortcutListener();
  useShortcut(
    "help",
    useCallback(() => setHelpOpen(true), []),
  );

  // Realtime cache invalidation over SSE, for agents and admins.
  useRealtimeEvents(user.role !== "customer");

  return (
    <div className="flex h-svh bg-background text-foreground">
      <Sidebar />
      <div className="flex min-w-0 flex-1 flex-col">
        <Topbar user={user} onOpenPalette={() => setPaletteOpen(true)} />
        <main className="min-h-0 flex-1 overflow-y-auto">
          <Outlet />
        </main>
      </div>
      <CommandPalette open={paletteOpen} onOpenChange={setPaletteOpen} />
      <ShortcutsHelpDialog open={helpOpen} onOpenChange={setHelpOpen} />
    </div>
  );
}
