import { useQuery } from "@tanstack/react-query";
import { useNavigate, useParams } from "@tanstack/react-router";
import { Copy, Moon, Plus, Sun, UserRound } from "lucide-react";
import { useState } from "react";

import { meQueryOptions } from "@/api/auth";
import { ticketQueryOptions, useUpdateTicket } from "@/api/tickets";
import type { TicketPriority, TicketStatus } from "@/api/types";
import {
  PRIORITY_LABELS,
  STATUS_LABELS,
  TICKET_PRIORITIES,
  TICKET_STATUSES,
} from "@/api/types";
import { navItemsFor } from "@/components/layout/nav";
import { PriorityIcon, StatusBadge } from "@/components/tickets/badges";
import { NewTicketDialog } from "@/components/tickets/new-ticket-dialog";
import { useTheme } from "@/components/theme";
import {
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  CommandSeparator,
} from "@/components/ui/command";

/**
 * cmdk palette: navigation, workspace actions, and — when a ticket is open —
 * context actions on that ticket (assign to me, status, priority, copy
 * number).
 */
export function CommandPalette({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const navigate = useNavigate();
  const { theme, toggleTheme } = useTheme();
  const [newTicketOpen, setNewTicketOpen] = useState(false);

  // The open ticket, if the detail route is active.
  const params = useParams({ strict: false });
  const ticketId = params.ticketId ?? null;
  const ticketQuery = useQuery({
    ...ticketQueryOptions(ticketId ?? "none"),
    enabled: open && ticketId !== null,
  });
  const ticket = ticketId !== null ? ticketQuery.data : undefined;

  const meQuery = useQuery({ ...meQueryOptions, enabled: open });
  const updateTicket = useUpdateTicket(ticketId ?? "none");

  function run(action: () => void) {
    onOpenChange(false);
    action();
  }

  function setStatus(status: TicketStatus) {
    if (!ticket || status === ticket.status) return;
    updateTicket.mutate({
      body: { status },
      optimistic: {
        status,
        closed_at: status === "closed" ? new Date().toISOString() : null,
      },
    });
  }

  function setPriority(priority: TicketPriority) {
    if (!ticket || priority === ticket.priority) return;
    updateTicket.mutate({ body: { priority }, optimistic: { priority } });
  }

  function assignToMe() {
    const me = meQuery.data;
    if (!ticket || !me || ticket.assignee?.id === me.id) return;
    updateTicket.mutate({
      body: { assignee_id: me.id },
      optimistic: {
        assignee: { id: me.id, name: me.name, email: me.email },
      },
    });
  }

  return (
    <>
      <CommandDialog
        open={open}
        onOpenChange={onOpenChange}
        title="Command palette"
        description="Type a command or search"
      >
        <CommandInput placeholder="Type a command or search…" />
        <CommandList>
          <CommandEmpty>No results found.</CommandEmpty>

          {ticket && (
            <>
              <CommandGroup heading={`Ticket ${ticket.number}`}>
                <CommandItem
                  value="assign to me take ticket"
                  onSelect={() => run(assignToMe)}
                >
                  <UserRound aria-hidden="true" />
                  Assign to me
                </CommandItem>
                <CommandItem
                  value="copy ticket number"
                  onSelect={() =>
                    run(() => {
                      void navigator.clipboard.writeText(ticket.number);
                    })
                  }
                >
                  <Copy aria-hidden="true" />
                  Copy ticket number
                </CommandItem>
                {TICKET_STATUSES.filter((s) => s !== ticket.status).map(
                  (status) => (
                    <CommandItem
                      key={status}
                      value={`set status ${STATUS_LABELS[status]}`}
                      onSelect={() => run(() => setStatus(status))}
                    >
                      <StatusBadge status={status} className="gap-2" />
                    </CommandItem>
                  ),
                )}
                {TICKET_PRIORITIES.filter((p) => p !== ticket.priority).map(
                  (priority) => (
                    <CommandItem
                      key={priority}
                      value={`set priority ${PRIORITY_LABELS[priority]}`}
                      onSelect={() => run(() => setPriority(priority))}
                    >
                      <PriorityIcon priority={priority} />
                      Priority: {PRIORITY_LABELS[priority]}
                    </CommandItem>
                  ),
                )}
              </CommandGroup>
              <CommandSeparator />
            </>
          )}

          <CommandGroup heading="Go to">
            {navItemsFor(meQuery.data?.role).map((item) => (
              <CommandItem
                key={item.to}
                onSelect={() => run(() => void navigate({ to: item.to }))}
              >
                <item.icon aria-hidden="true" />
                {item.label}
                {item.soon && (
                  <span className="ml-auto text-xs text-muted-foreground">
                    soon
                  </span>
                )}
              </CommandItem>
            ))}
          </CommandGroup>
          <CommandSeparator />
          <CommandGroup heading="Actions">
            <CommandItem
              value="new ticket create"
              onSelect={() => run(() => setNewTicketOpen(true))}
            >
              <Plus aria-hidden="true" />
              New ticket
            </CommandItem>
          </CommandGroup>
          <CommandSeparator />
          <CommandGroup heading="Preferences">
            <CommandItem onSelect={() => run(toggleTheme)}>
              {theme === "dark" ? (
                <Sun aria-hidden="true" />
              ) : (
                <Moon aria-hidden="true" />
              )}
              Switch to {theme === "dark" ? "light" : "dark"} mode
            </CommandItem>
          </CommandGroup>
        </CommandList>
      </CommandDialog>

      <NewTicketDialog open={newTicketOpen} onOpenChange={setNewTicketOpen} />
    </>
  );
}
