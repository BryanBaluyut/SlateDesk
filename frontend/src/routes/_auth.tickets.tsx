import { createFileRoute, Outlet } from "@tanstack/react-router";
import { z } from "zod";

import { QueuePane } from "@/components/tickets/queue-pane";

/**
 * Tickets workspace layout: LEFT fixed queue pane, everything else (empty
 * state or thread + context sidebar) in the outlet. Queue state — fixed view,
 * priority/tag filter chips, FTS query — lives in URL search params so views
 * are linkable and survive reloads.
 */

const ticketsSearchSchema = z.object({
  // Absent = the default "All open" view (applied in the queue pane), so
  // plain links to /tickets need no search params.
  view: z.enum(["my", "unassigned", "open", "closed"]).optional().catch(undefined),
  // Dashboard tiles deep-link e.g. view=open&status=waiting_on_customer;
  // the queue pane renders active status filters as removable chips so the
  // narrowing is always visible.
  status: z
    .array(z.enum(["open", "waiting_on_customer", "on_hold", "closed"]))
    .optional()
    .catch(undefined),
  // Deep link for the dashboard's "Closed today" tile (same predicate as
  // its counter); rendered as a removable chip like status.
  closed_today: z.boolean().optional().catch(undefined),
  priority: z
    .array(z.enum(["low", "medium", "high", "critical"]))
    .optional()
    .catch(undefined),
  tag: z.string().optional().catch(undefined),
  q: z.string().optional().catch(undefined),
});

export type TicketsSearch = z.infer<typeof ticketsSearchSchema>;

export const Route = createFileRoute("/_auth/tickets")({
  validateSearch: (search) => ticketsSearchSchema.parse(search),
  component: TicketsLayout,
});

function TicketsLayout() {
  return (
    <div className="flex h-full min-h-0">
      <QueuePane />
      <div className="flex min-w-0 flex-1 flex-col">
        <Outlet />
      </div>
    </div>
  );
}
