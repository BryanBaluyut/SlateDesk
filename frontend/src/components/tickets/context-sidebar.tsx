import { useQuery } from "@tanstack/react-query";
import { Check, ChevronsUpDown, Plus, UserRound } from "lucide-react";
import { useCallback, useState } from "react";

import { meQueryOptions } from "@/api/auth";
import { api } from "@/api/client";
import { tagsQueryOptions, useCreateTag } from "@/api/tags";
import { useSetTicketTags, useUpdateTicket } from "@/api/tickets";
import { teamsQueryOptions } from "@/api/teams";
import type {
  TicketDetail,
  TicketPriority,
  TicketStatus,
  User,
} from "@/api/types";
import {
  PRIORITY_LABELS,
  STATUS_LABELS,
  TICKET_PRIORITIES,
  TICKET_STATUSES,
} from "@/api/types";
import { agentsQueryOptions } from "@/api/users";
import { initials } from "@/components/layout/topbar";
import { PriorityIcon, TagChip } from "@/components/tickets/badges";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Button } from "@/components/ui/button";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { useShortcut } from "@/lib/shortcuts";
import { formatFullTime } from "@/lib/time";
import { cn } from "@/lib/utils";

/**
 * RIGHT context sidebar. Stays mounted while collapsed (CSS-hidden) so the
 * s / p / a shortcuts can expand it and open the matching control in one
 * keystroke. Every change optimistic-updates the detail cache and
 * invalidates lists + counters on settle.
 */
export function ContextSidebar({
  ticket,
  collapsed,
  onExpand,
}: {
  ticket: TicketDetail;
  collapsed: boolean;
  onExpand: () => void;
}) {
  const updateTicket = useUpdateTicket(ticket.id);
  const setTicketTags = useSetTicketTags(ticket.id);
  const createTag = useCreateTag();

  const meQuery = useQuery(meQueryOptions);
  const agentsQuery = useQuery({ ...agentsQueryOptions, retry: false });
  const teamsQuery = useQuery(teamsQueryOptions);
  const tagsQuery = useQuery(tagsQueryOptions);

  // Best-effort requester enrichment (company lives on the full user, not
  // the embedded summary). 403/404 just means we show the summary.
  const requesterQuery = useQuery({
    queryKey: ["users", "one", ticket.requester.id],
    queryFn: () => api.get<User>(`/users/${ticket.requester.id}`),
    retry: false,
    staleTime: 5 * 60_000,
  });

  const [statusOpen, setStatusOpen] = useState(false);
  const [priorityOpen, setPriorityOpen] = useState(false);
  const [assigneeOpen, setAssigneeOpen] = useState(false);
  const [tagsOpen, setTagsOpen] = useState(false);
  const [tagQuery, setTagQuery] = useState("");

  /** Shortcut target: expand the sidebar if needed, then open a control. */
  const openControl = useCallback(
    (setter: (open: boolean) => void) => {
      if (collapsed) onExpand();
      // Let the expand render before the overlay measures its trigger.
      requestAnimationFrame(() => requestAnimationFrame(() => setter(true)));
    },
    [collapsed, onExpand],
  );

  useShortcut(
    "status",
    useCallback(() => openControl(setStatusOpen), [openControl]),
  );
  useShortcut(
    "priority",
    useCallback(() => openControl(setPriorityOpen), [openControl]),
  );
  useShortcut(
    "assign",
    useCallback(() => openControl(setAssigneeOpen), [openControl]),
  );

  function setStatus(status: TicketStatus) {
    if (status === ticket.status) return;
    updateTicket.mutate({
      body: { status },
      optimistic: {
        status,
        closed_at: status === "closed" ? new Date().toISOString() : null,
      },
    });
  }

  function setPriority(priority: TicketPriority) {
    if (priority === ticket.priority) return;
    updateTicket.mutate({ body: { priority }, optimistic: { priority } });
  }

  function setAssignee(user: { id: string; name: string; email: string } | null) {
    setAssigneeOpen(false);
    if ((user?.id ?? null) === (ticket.assignee?.id ?? null)) return;
    updateTicket.mutate({
      body: { assignee_id: user?.id ?? null },
      optimistic: {
        assignee: user
          ? { id: user.id, name: user.name, email: user.email }
          : null,
      },
    });
  }

  function setTeam(teamId: string | null) {
    if (teamId === ticket.team_id) return;
    updateTicket.mutate({
      body: { team_id: teamId },
      optimistic: { team_id: teamId },
    });
  }

  function toggleTag(tagId: string) {
    const current = ticket.tags.map((t) => t.id);
    const next = current.includes(tagId)
      ? current.filter((id) => id !== tagId)
      : [...current, tagId];
    const all = tagsQuery.data ?? [];
    const optimisticTags = all
      .filter((t) => next.includes(t.id))
      .sort((a, b) => a.name.localeCompare(b.name));
    setTicketTags.mutate({ tagIds: next, optimisticTags });
  }

  async function createAndAttachTag(name: string) {
    const trimmed = name.trim();
    if (!trimmed) return;
    setTagQuery("");
    try {
      const tag = await createTag.mutateAsync({ name: trimmed });
      const next = [...ticket.tags.map((t) => t.id), tag.id];
      const optimisticTags = [...ticket.tags, tag].sort((a, b) =>
        a.name.localeCompare(b.name),
      );
      setTicketTags.mutate({ tagIds: next, optimisticTags });
    } catch {
      // Tag may already exist (409 race) — the tags list refetch covers it.
    }
  }

  const me = meQuery.data;
  const agents = agentsQuery.data ?? [];
  const teams = teamsQuery.data ?? [];
  const allTags = tagsQuery.data ?? [];
  const requester = requesterQuery.data;

  const tagQueryTrimmed = tagQuery.trim();
  const tagExists = allTags.some(
    (t) => t.name.toLowerCase() === tagQueryTrimmed.toLowerCase(),
  );

  return (
    <aside
      aria-label="Ticket context"
      className={cn(
        "w-72 shrink-0 overflow-y-auto border-l",
        collapsed && "hidden",
      )}
    >
      <div className="space-y-4 p-4">
        {/* Requester card */}
        <div className="rounded-lg border p-3">
          <div className="flex items-center gap-2.5">
            <Avatar className="size-8">
              <AvatarFallback>
                {initials(ticket.requester.name, ticket.requester.email)}
              </AvatarFallback>
            </Avatar>
            <div className="min-w-0">
              <div className="truncate text-sm font-medium">
                {ticket.requester.name || "—"}
              </div>
              <div className="truncate text-xs text-muted-foreground">
                {ticket.requester.email}
              </div>
            </div>
          </div>
          {requester?.company && (
            <div className="mt-2 flex items-center gap-1.5 text-xs text-muted-foreground">
              <UserRound className="size-3" aria-hidden="true" />
              {requester.company}
            </div>
          )}
        </div>

        {/* Status */}
        <Field label="Status" hint="s">
          <Select
            open={statusOpen}
            onOpenChange={setStatusOpen}
            value={ticket.status}
            onValueChange={(v) => setStatus(v as TicketStatus)}
          >
            <SelectTrigger size="sm" className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {TICKET_STATUSES.map((status) => (
                <SelectItem key={status} value={status}>
                  {STATUS_LABELS[status]}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>

        {/* Priority */}
        <Field label="Priority" hint="p">
          <Select
            open={priorityOpen}
            onOpenChange={setPriorityOpen}
            value={ticket.priority}
            onValueChange={(v) => setPriority(v as TicketPriority)}
          >
            <SelectTrigger size="sm" className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {TICKET_PRIORITIES.map((priority) => (
                <SelectItem key={priority} value={priority}>
                  <PriorityIcon priority={priority} />
                  {PRIORITY_LABELS[priority]}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>

        {/* Assignee */}
        <Field label="Assignee" hint="a">
          <Popover open={assigneeOpen} onOpenChange={setAssigneeOpen}>
            <PopoverTrigger asChild>
              <Button
                variant="outline"
                size="sm"
                role="combobox"
                aria-expanded={assigneeOpen}
                className="w-full justify-between font-normal"
              >
                {ticket.assignee ? (
                  <span className="flex min-w-0 items-center gap-2">
                    <Avatar className="size-5">
                      <AvatarFallback className="text-[9px]">
                        {initials(
                          ticket.assignee.name,
                          ticket.assignee.email,
                        )}
                      </AvatarFallback>
                    </Avatar>
                    <span className="truncate">
                      {ticket.assignee.name || ticket.assignee.email}
                    </span>
                  </span>
                ) : (
                  <span className="text-muted-foreground">Unassigned</span>
                )}
                <ChevronsUpDown
                  className="size-3.5 shrink-0 opacity-50"
                  aria-hidden="true"
                />
              </Button>
            </PopoverTrigger>
            <PopoverContent className="w-64 p-0" align="start">
              <Command>
                <CommandInput placeholder="Search agents…" />
                <CommandList>
                  <CommandEmpty>
                    {agentsQuery.isError
                      ? "Could not load the agent list."
                      : "No matching agent."}
                  </CommandEmpty>
                  <CommandGroup>
                    {me && (
                      <CommandItem
                        value={`me ${me.name} ${me.email}`}
                        onSelect={() => setAssignee(me)}
                      >
                        <UserRound aria-hidden="true" />
                        Assign to me
                      </CommandItem>
                    )}
                    <CommandItem
                      value="unassigned nobody clear"
                      onSelect={() => setAssignee(null)}
                    >
                      <Check
                        aria-hidden="true"
                        className={cn(!ticket.assignee || "invisible")}
                      />
                      Unassigned
                    </CommandItem>
                    {agents.map((agent) => (
                      <CommandItem
                        key={agent.id}
                        value={`${agent.name} ${agent.email}`}
                        onSelect={() => setAssignee(agent)}
                      >
                        <Check
                          aria-hidden="true"
                          className={cn(
                            ticket.assignee?.id === agent.id || "invisible",
                          )}
                        />
                        <span className="truncate">
                          {agent.name || agent.email}
                        </span>
                      </CommandItem>
                    ))}
                  </CommandGroup>
                </CommandList>
              </Command>
            </PopoverContent>
          </Popover>
        </Field>

        {/* Team */}
        <Field label="Team">
          <Select
            value={ticket.team_id ?? "none"}
            onValueChange={(v) => setTeam(v === "none" ? null : v)}
          >
            <SelectTrigger size="sm" className="w-full">
              <SelectValue placeholder="No team" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="none">
                <span className="text-muted-foreground">No team</span>
              </SelectItem>
              {teams.map((team) => (
                <SelectItem key={team.id} value={team.id}>
                  {team.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>

        {/* Tags */}
        <Field label="Tags">
          <div className="flex flex-wrap gap-1.5">
            {ticket.tags.map((tag) => (
              <TagChip
                key={tag.id}
                tag={tag}
                onClick={() => toggleTag(tag.id)}
                active
              />
            ))}
            <Popover
              open={tagsOpen}
              onOpenChange={(open) => {
                setTagsOpen(open);
                if (!open) setTagQuery("");
              }}
            >
              <PopoverTrigger asChild>
                <Button
                  variant="outline"
                  size="xs"
                  className="rounded-full font-normal text-muted-foreground"
                >
                  <Plus aria-hidden="true" />
                  Add tag
                </Button>
              </PopoverTrigger>
              <PopoverContent className="w-64 p-0" align="start">
                <Command>
                  <CommandInput
                    placeholder="Search or create…"
                    value={tagQuery}
                    onValueChange={setTagQuery}
                  />
                  <CommandList>
                    <CommandEmpty>No tags yet — type to create.</CommandEmpty>
                    <CommandGroup>
                      {allTags.map((tag) => {
                        const attached = ticket.tags.some(
                          (t) => t.id === tag.id,
                        );
                        return (
                          <CommandItem
                            key={tag.id}
                            value={tag.name}
                            onSelect={() => toggleTag(tag.id)}
                          >
                            <Check
                              aria-hidden="true"
                              className={cn(attached || "invisible")}
                            />
                            <span
                              className="size-2 shrink-0 rounded-full"
                              style={{
                                backgroundColor:
                                  tag.color ?? "var(--muted-foreground)",
                              }}
                              aria-hidden="true"
                            />
                            <span className="truncate">{tag.name}</span>
                          </CommandItem>
                        );
                      })}
                      {tagQueryTrimmed && !tagExists && (
                        <CommandItem
                          value={`create-${tagQueryTrimmed}`}
                          onSelect={() =>
                            void createAndAttachTag(tagQueryTrimmed)
                          }
                        >
                          <Plus aria-hidden="true" />
                          Create “{tagQueryTrimmed}”
                        </CommandItem>
                      )}
                    </CommandGroup>
                  </CommandList>
                </Command>
              </PopoverContent>
            </Popover>
          </div>
        </Field>

        <Separator />

        <dl className="space-y-1 text-xs text-muted-foreground">
          <div className="flex justify-between gap-2">
            <dt>Created</dt>
            <dd>{formatFullTime(ticket.created_at)}</dd>
          </div>
          <div className="flex justify-between gap-2">
            <dt>Updated</dt>
            <dd>{formatFullTime(ticket.updated_at)}</dd>
          </div>
          {ticket.closed_at && (
            <div className="flex justify-between gap-2">
              <dt>Closed</dt>
              <dd>{formatFullTime(ticket.closed_at)}</dd>
            </div>
          )}
        </dl>
      </div>
    </aside>
  );
}

function Field({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="space-y-1.5">
      <div className="flex items-baseline justify-between">
        <span className="text-xs font-medium text-muted-foreground">
          {label}
        </span>
        {hint && (
          <kbd className="rounded border bg-muted px-1 font-mono text-[10px] text-muted-foreground">
            {hint}
          </kbd>
        )}
      </div>
      {children}
    </div>
  );
}
