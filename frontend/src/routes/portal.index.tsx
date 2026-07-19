import { useSuspenseQuery } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { Plus, Ticket as TicketIcon } from "lucide-react";

import { portalTicketsQueryOptions } from "@/api/portal";
import type { PortalTicket } from "@/api/types";
import { STATUS_LABELS } from "@/api/types";
import { Button } from "@/components/ui/button";
import { formatRelativeTime } from "@/lib/time";

export const Route = createFileRoute("/portal/")({
  loader: ({ context }) =>
    context.queryClient.ensureQueryData(portalTicketsQueryOptions),
  component: MyTickets,
});

function MyTickets() {
  const { data: tickets } = useSuspenseQuery(portalTicketsQueryOptions);

  return (
    <div className="space-y-5">
      <div className="flex items-center justify-between">
        <h1 className="text-xl font-semibold tracking-tight">My requests</h1>
        <Button asChild size="sm">
          <Link to="/portal/new">
            <Plus className="size-4" aria-hidden="true" />
            New request
          </Link>
        </Button>
      </div>

      {tickets.length === 0 ? (
        <EmptyState />
      ) : (
        <ul className="divide-y overflow-hidden rounded-xl border bg-background">
          {tickets.map((t) => (
            <TicketRow key={t.id} ticket={t} />
          ))}
        </ul>
      )}
    </div>
  );
}

function TicketRow({ ticket }: { ticket: PortalTicket }) {
  const closed = ticket.status === "closed";
  return (
    <li>
      <Link
        to="/portal/tickets/$ticketId"
        params={{ ticketId: ticket.id }}
        className="flex items-center justify-between gap-4 px-4 py-3 transition-colors hover:bg-muted/50"
      >
        <div className="min-w-0">
          <p className="truncate text-sm font-medium">{ticket.subject}</p>
          <p className="mt-0.5 text-xs text-muted-foreground">
            {ticket.number} · updated {formatRelativeTime(ticket.updated_at)}
          </p>
        </div>
        <span
          className={
            "shrink-0 rounded-full px-2.5 py-0.5 text-xs font-medium " +
            (closed
              ? "bg-muted text-muted-foreground"
              : "bg-primary/10 text-primary")
          }
        >
          {STATUS_LABELS[ticket.status]}
        </span>
      </Link>
    </li>
  );
}

function EmptyState() {
  return (
    <div className="flex flex-col items-center gap-3 rounded-xl border border-dashed bg-background px-6 py-12 text-center">
      <div className="flex size-10 items-center justify-center rounded-full border bg-muted/50">
        <TicketIcon className="size-5 text-muted-foreground" aria-hidden="true" />
      </div>
      <div>
        <h2 className="text-sm font-medium">No requests yet</h2>
        <p className="mt-1 text-sm text-muted-foreground">
          Open a request and we&apos;ll help you out.
        </p>
      </div>
      <Button asChild size="sm" className="mt-1">
        <Link to="/portal/new">
          <Plus className="size-4" aria-hidden="true" />
          New request
        </Link>
      </Button>
    </div>
  );
}
