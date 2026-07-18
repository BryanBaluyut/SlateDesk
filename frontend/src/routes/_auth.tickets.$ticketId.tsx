import { useSuspenseQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import { useCallback, useState } from "react";

import { ticketQueryOptions } from "@/api/tickets";
import { ContextSidebar } from "@/components/tickets/context-sidebar";
import { ThreadPane } from "@/components/tickets/thread-pane";
import { useShortcut } from "@/lib/shortcuts";

const SIDEBAR_KEY = "slatedesk-ticket-sidebar-collapsed";

export const Route = createFileRoute("/_auth/tickets/$ticketId")({
  loader: ({ context, params }) =>
    context.queryClient.ensureQueryData(ticketQueryOptions(params.ticketId)),
  component: TicketDetailPage,
});

/**
 * CENTER thread pane + RIGHT collapsible context sidebar. The sidebar stays
 * mounted while collapsed (hidden via CSS) so its keyboard-openable controls
 * (s / p / a) can expand it and open in one keystroke.
 */
function TicketDetailPage() {
  const { ticketId } = Route.useParams();
  const ticketQuery = useSuspenseQuery(ticketQueryOptions(ticketId));
  const ticket = ticketQuery.data;

  const [collapsed, setCollapsed] = useState(
    () => localStorage.getItem(SIDEBAR_KEY) === "1",
  );

  const toggleSidebar = useCallback(() => {
    setCollapsed((c) => {
      localStorage.setItem(SIDEBAR_KEY, c ? "0" : "1");
      return !c;
    });
  }, []);

  const expandSidebar = useCallback(() => {
    localStorage.setItem(SIDEBAR_KEY, "0");
    setCollapsed(false);
  }, []);

  useShortcut("sidebar", toggleSidebar);

  return (
    <div className="flex h-full min-h-0 min-w-0">
      <ThreadPane
        ticket={ticket}
        sidebarCollapsed={collapsed}
        onToggleSidebar={toggleSidebar}
      />
      <ContextSidebar
        ticket={ticket}
        collapsed={collapsed}
        onExpand={expandSidebar}
      />
    </div>
  );
}
