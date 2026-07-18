import { useQuery } from "@tanstack/react-query";
import {
  getRouteApi,
  Link,
  useNavigate,
  useParams,
} from "@tanstack/react-router";
import { Plus, Search, X } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { tagsQueryOptions } from "@/api/tags";
import { ticketsQueryOptions, TICKETS_PAGE_SIZE } from "@/api/tickets";
import type {
  Ticket,
  TicketPriority,
  TicketStatus,
  TicketView,
} from "@/api/types";
import {
  PRIORITY_LABELS,
  STATUS_LABELS,
  TICKET_PRIORITIES,
  TICKET_VIEWS,
  VIEW_LABELS,
} from "@/api/types";
import { initials } from "@/components/layout/topbar";
import { PriorityIcon, StatusBadge } from "@/components/tickets/badges";
import { NewTicketDialog } from "@/components/tickets/new-ticket-dialog";
import { ProblemAlert } from "@/components/problem-alert";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { useShortcut } from "@/lib/shortcuts";
import { formatFullTime, formatRelativeTime } from "@/lib/time";
import { cn } from "@/lib/utils";
import type { TicketsSearch } from "@/routes/_auth.tickets";

const route = getRouteApi("/_auth/tickets");

const VIEW_SHORT_LABELS: Record<TicketView, string> = {
  my: "Mine",
  unassigned: "Unassigned",
  open: "Open",
  closed: "Closed",
};

export function QueuePane() {
  const search = route.useSearch();
  const navigate = useNavigate();
  const params = useParams({ strict: false });
  const openTicketId = params.ticketId ?? null;

  /**
   * Update the queue's URL search params without leaving the currently open
   * ticket (search params live on the layout; the path must be preserved
   * explicitly).
   */
  const updateSearch = useCallback(
    (updater: (prev: TicketsSearch) => TicketsSearch, replace = false) => {
      if (openTicketId !== null) {
        void navigate({
          to: "/tickets/$ticketId",
          params: { ticketId: openTicketId },
          search: updater,
          replace,
        });
      } else {
        void navigate({ to: "/tickets", search: updater, replace });
      }
    },
    [navigate, openTicketId],
  );

  /** Absent view param = the default "All open" view. */
  const view = search.view ?? "open";

  const filters = useMemo(
    () => ({
      view,
      status: search.status,
      priority: search.priority,
      tag_id: search.tag,
      closed_today: search.closed_today,
      q: search.q,
    }),
    [
      view,
      search.status,
      search.priority,
      search.tag,
      search.closed_today,
      search.q,
    ],
  );

  const ticketsQuery = useQuery(ticketsQueryOptions(filters));
  const tagsQuery = useQuery(tagsQueryOptions);
  const tickets = useMemo(
    () => ticketsQuery.data?.items ?? [],
    [ticketsQuery.data],
  );
  const total = ticketsQuery.data?.total ?? 0;

  const [newTicketOpen, setNewTicketOpen] = useState(false);

  // --- keyboard cursor -----------------------------------------------------
  const [cursor, setCursor] = useState(0);
  const listRef = useRef<HTMLDivElement>(null);
  const searchInputRef = useRef<HTMLInputElement>(null);

  // Clamp the cursor whenever the list changes shape.
  useEffect(() => {
    setCursor((c) => Math.max(0, Math.min(c, tickets.length - 1)));
  }, [tickets.length]);

  const moveCursor = useCallback(
    (delta: number) => {
      if (tickets.length === 0) return;
      setCursor((c) => {
        const next = Math.max(0, Math.min(tickets.length - 1, c + delta));
        const row = listRef.current?.querySelector(`[data-index="${next}"]`);
        row?.scrollIntoView({ block: "nearest" });
        return next;
      });
    },
    [tickets.length],
  );

  const openCursorTicket = useCallback(() => {
    const ticket = tickets[cursor];
    if (!ticket) return;
    void navigate({
      to: "/tickets/$ticketId",
      params: { ticketId: ticket.id },
      search: (prev) => prev,
    });
  }, [tickets, cursor, navigate]);

  useShortcut("queue-down", useCallback(() => moveCursor(1), [moveCursor]));
  useShortcut("queue-up", useCallback(() => moveCursor(-1), [moveCursor]));
  useShortcut("queue-open", openCursorTicket);
  useShortcut(
    "search",
    useCallback(() => searchInputRef.current?.focus(), []),
  );

  // --- debounced search box ------------------------------------------------
  const [queryText, setQueryText] = useState(search.q ?? "");
  const urlQ = search.q ?? "";
  // Adopt external changes (palette navigation, back button).
  const lastUrlQ = useRef(urlQ);
  useEffect(() => {
    if (urlQ !== lastUrlQ.current) {
      lastUrlQ.current = urlQ;
      setQueryText(urlQ);
    }
  }, [urlQ]);
  useEffect(() => {
    if (queryText === urlQ) return;
    const timer = setTimeout(() => {
      lastUrlQ.current = queryText;
      updateSearch((prev) => ({ ...prev, q: queryText || undefined }), true);
    }, 300);
    return () => clearTimeout(timer);
  }, [queryText, urlQ, updateSearch]);

  // --- filter helpers ------------------------------------------------------
  function setView(view: TicketView) {
    // Clear the deep-link filters (status, closed-today): they would
    // contradict the newly chosen view.
    updateSearch((prev) => ({
      ...prev,
      view,
      status: undefined,
      closed_today: undefined,
    }));
  }

  function removeStatus(status: TicketStatus) {
    updateSearch((prev) => {
      const next = (prev.status ?? []).filter((s) => s !== status);
      return { ...prev, status: next.length > 0 ? next : undefined };
    });
  }

  function togglePriority(p: TicketPriority) {
    updateSearch((prev) => {
      const current = prev.priority ?? [];
      const next = current.includes(p)
        ? current.filter((x) => x !== p)
        : [...current, p];
      return { ...prev, priority: next.length > 0 ? next : undefined };
    });
  }

  function toggleTag(id: string) {
    updateSearch((prev) => ({
      ...prev,
      tag: prev.tag === id ? undefined : id,
    }));
  }

  const tags = tagsQuery.data ?? [];

  return (
    <section
      aria-label="Ticket queue"
      className="flex w-80 shrink-0 flex-col border-r xl:w-88"
    >
      {/* Fixed views. aria-pressed toggle buttons, NOT role=tab: these are
          ordinary Tab-reachable buttons without the ARIA tabs pattern's
          arrow-key/roving-tabindex behavior (and there is no tabpanel), so
          tab semantics would announce an interaction model that lies. */}
      <div className="flex items-center gap-1 border-b px-2 py-2">
        <div
          role="group"
          aria-label="Fixed views"
          className="flex flex-1 gap-0.5"
        >
          {TICKET_VIEWS.map((v) => (
            <button
              key={v}
              type="button"
              aria-pressed={view === v}
              title={VIEW_LABELS[v]}
              onClick={() => setView(v)}
              className={cn(
                "rounded-md px-2 py-1 text-xs font-medium transition-colors",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                view === v
                  ? "bg-accent text-accent-foreground"
                  : "text-muted-foreground hover:bg-accent/60 hover:text-accent-foreground",
              )}
            >
              {VIEW_SHORT_LABELS[v]}
            </button>
          ))}
        </div>
        <Button
          variant="ghost"
          size="icon-xs"
          aria-label="New ticket"
          title="New ticket"
          onClick={() => setNewTicketOpen(true)}
        >
          <Plus aria-hidden="true" />
        </Button>
      </div>

      {/* Search */}
      <div className="border-b px-2 py-2">
        <div className="relative">
          <Search
            className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground"
            aria-hidden="true"
          />
          <Input
            ref={searchInputRef}
            value={queryText}
            onChange={(e) => setQueryText(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Escape") e.currentTarget.blur();
            }}
            placeholder="Search tickets…  /"
            aria-label="Search tickets"
            className="h-8 pl-8 text-sm"
          />
        </div>
      </div>

      {/* Filter chips */}
      <div className="flex flex-wrap gap-1 border-b px-2 py-2">
        {/* Deep-link filters (dashboard tiles): without a visible,
            removable chip these silently narrow the queue under an
            innocent-looking view tab. */}
        {(search.status ?? []).map((s) => (
          <button
            key={`status-${s}`}
            type="button"
            onClick={() => removeStatus(s)}
            aria-label={`Remove status filter: ${STATUS_LABELS[s]}`}
            className={cn(
              "inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-xs transition-colors",
              "border-primary/40 bg-primary/10 text-foreground hover:bg-primary/20",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
            )}
          >
            {STATUS_LABELS[s]}
            <X className="size-3" aria-hidden="true" />
          </button>
        ))}
        {search.closed_today && (
          <button
            type="button"
            onClick={() =>
              updateSearch((prev) => ({ ...prev, closed_today: undefined }))
            }
            aria-label="Remove filter: closed today"
            className={cn(
              "inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-xs transition-colors",
              "border-primary/40 bg-primary/10 text-foreground hover:bg-primary/20",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
            )}
          >
            Closed today
            <X className="size-3" aria-hidden="true" />
          </button>
        )}
        {TICKET_PRIORITIES.map((p) => {
          const active = (search.priority ?? []).includes(p);
          return (
            <button
              key={p}
              type="button"
              onClick={() => togglePriority(p)}
              aria-pressed={active}
              className={cn(
                "inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-xs transition-colors",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                active
                  ? "border-primary/40 bg-primary/10 text-foreground"
                  : "text-muted-foreground hover:bg-accent hover:text-accent-foreground",
              )}
            >
              <PriorityIcon priority={p} />
              {PRIORITY_LABELS[p]}
            </button>
          );
        })}
        {tags.map((tag) => {
          const active = search.tag === tag.id;
          return (
            <button
              key={tag.id}
              type="button"
              onClick={() => toggleTag(tag.id)}
              aria-pressed={active}
              className={cn(
                "inline-flex max-w-36 items-center gap-1.5 rounded-full border px-2 py-0.5 text-xs transition-colors",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                active
                  ? "border-primary/40 bg-primary/10 text-foreground"
                  : "text-muted-foreground hover:bg-accent hover:text-accent-foreground",
              )}
            >
              <span
                className="size-1.5 shrink-0 rounded-full"
                style={{
                  backgroundColor: tag.color ?? "var(--muted-foreground)",
                }}
                aria-hidden="true"
              />
              <span className="truncate">{tag.name}</span>
            </button>
          );
        })}
      </div>

      {/* Ticket list */}
      <div ref={listRef} className="min-h-0 flex-1 overflow-y-auto">
        {ticketsQuery.isPending && (
          <div className="space-y-2 p-2">
            {Array.from({ length: 6 }, (_, i) => (
              <Skeleton key={i} className="h-16 w-full" />
            ))}
          </div>
        )}

        {ticketsQuery.isError && (
          <div className="p-2">
            <ProblemAlert error={ticketsQuery.error} />
          </div>
        )}

        {ticketsQuery.isSuccess && tickets.length === 0 && (
          <p className="p-6 text-center text-sm text-muted-foreground">
            {search.q
              ? "No tickets match this search."
              : "No tickets in this view."}
          </p>
        )}

        {tickets.map((ticket, index) => (
          <QueueRow
            key={ticket.id}
            ticket={ticket}
            index={index}
            isCursor={index === cursor}
            onHover={() => setCursor(index)}
          />
        ))}

        {ticketsQuery.isSuccess && total > tickets.length && (
          <p className="border-t px-3 py-2 text-center text-xs text-muted-foreground">
            Showing first {TICKETS_PAGE_SIZE} of {total} — refine the filters
            to narrow down.
          </p>
        )}
      </div>

      <div className="border-t px-3 py-1.5 text-xs text-muted-foreground">
        {ticketsQuery.isSuccess
          ? `${total} ${total === 1 ? "ticket" : "tickets"}`
          : "…"}
      </div>

      <NewTicketDialog open={newTicketOpen} onOpenChange={setNewTicketOpen} />
    </section>
  );
}

function QueueRow({
  ticket,
  index,
  isCursor,
  onHover,
}: {
  ticket: Ticket;
  index: number;
  isCursor: boolean;
  onHover: () => void;
}) {
  return (
    <Link
      to="/tickets/$ticketId"
      params={{ ticketId: ticket.id }}
      search={(prev) => prev}
      data-index={index}
      onMouseMove={onHover}
      className={cn(
        "block border-b px-3 py-2.5 text-sm transition-colors",
        "hover:bg-accent/50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset",
        isCursor && "bg-accent/40 shadow-[inset_2px_0_0_var(--primary)]",
      )}
      activeProps={{
        className: "bg-accent shadow-[inset_2px_0_0_var(--primary)]",
        "aria-current": "page",
      }}
    >
      <div className="flex items-center gap-2">
        <span className="font-mono text-[11px] text-muted-foreground">
          {ticket.number}
        </span>
        <PriorityIcon priority={ticket.priority} />
        <span
          className="ml-auto shrink-0 text-[11px] text-muted-foreground"
          title={formatFullTime(ticket.updated_at)}
        >
          {formatRelativeTime(ticket.updated_at)}
        </span>
      </div>
      <div className="mt-0.5 flex items-center gap-2">
        <span className="min-w-0 flex-1 truncate font-medium">
          {ticket.subject}
        </span>
        {ticket.assignee && (
          <Avatar
            className="size-5 shrink-0"
            title={`Assigned to ${ticket.assignee.name || ticket.assignee.email}`}
          >
            <AvatarFallback className="text-[9px]">
              {initials(ticket.assignee.name, ticket.assignee.email)}
            </AvatarFallback>
          </Avatar>
        )}
      </div>
      <div className="mt-1 flex items-center gap-2">
        <span className="min-w-0 truncate text-xs text-muted-foreground">
          {ticket.requester.name || ticket.requester.email}
        </span>
        <StatusBadge status={ticket.status} short className="ml-auto shrink-0" />
      </div>
    </Link>
  );
}
