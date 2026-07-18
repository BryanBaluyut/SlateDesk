import { useQuery } from "@tanstack/react-query";
import { Check, Copy, PanelRight, Paperclip } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";

import { meQueryOptions } from "@/api/auth";
import { attachmentDownloadUrl } from "@/api/attachments";
import { teamsQueryOptions } from "@/api/teams";
import type { Article, TicketDetail, TicketEvent } from "@/api/types";
import { PRIORITY_LABELS, STATUS_LABELS } from "@/api/types";
import { agentsQueryOptions } from "@/api/users";
import { initials } from "@/components/layout/topbar";
import { PriorityBadge, StatusBadge } from "@/components/tickets/badges";
import { Composer } from "@/components/tickets/composer";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Button } from "@/components/ui/button";
import { formatBytes, formatFullTime, formatThreadTime } from "@/lib/time";
import { cn } from "@/lib/utils";

type ThreadItem =
  | { kind: "article"; at: string; article: Article }
  | { kind: "event"; at: string; event: TicketEvent };

/** Audit types that render inline; created/article_added are redundant with
 * the articles themselves. */
const VISIBLE_EVENT_TYPES = new Set([
  "status_changed",
  "priority_changed",
  "assignee_changed",
  "team_changed",
  "tags_changed",
]);

export function ThreadPane({
  ticket,
  sidebarCollapsed,
  onToggleSidebar,
}: {
  ticket: TicketDetail;
  sidebarCollapsed: boolean;
  onToggleSidebar: () => void;
}) {
  const agentsQuery = useQuery({ ...agentsQueryOptions, retry: false });
  const teamsQuery = useQuery(teamsQueryOptions);
  const meQuery = useQuery(meQueryOptions);

  /** uuid → display name, for audit-event actors and payloads. */
  const names = useMemo(() => {
    const map = new Map<string, string>();
    const add = (id: string, name: string, email?: string) => {
      if (!map.has(id)) map.set(id, name || email || "someone");
    };
    add(ticket.requester.id, ticket.requester.name, ticket.requester.email);
    if (ticket.assignee) {
      add(ticket.assignee.id, ticket.assignee.name, ticket.assignee.email);
    }
    for (const article of ticket.articles) {
      if (article.author) {
        add(article.author.id, article.author.name, article.author.email);
      }
    }
    for (const user of agentsQuery.data ?? []) {
      add(user.id, user.name, user.email);
    }
    if (meQuery.data) {
      add(meQuery.data.id, meQuery.data.name, meQuery.data.email);
    }
    return map;
  }, [ticket, agentsQuery.data, meQuery.data]);

  const teamNames = useMemo(() => {
    const map = new Map<string, string>();
    for (const team of teamsQuery.data ?? []) map.set(team.id, team.name);
    return map;
  }, [teamsQuery.data]);

  const items = useMemo<ThreadItem[]>(() => {
    const merged: ThreadItem[] = [
      ...ticket.articles.map((article) => ({
        kind: "article" as const,
        at: article.created_at,
        article,
      })),
      ...ticket.events
        .filter((event) => VISIBLE_EVENT_TYPES.has(event.type))
        .map((event) => ({
          kind: "event" as const,
          at: event.created_at,
          event,
        })),
    ];
    // Chronological by epoch millis — comparing the raw RFC3339 strings
    // mis-orders timestamps whose fractional-second lengths differ (Go
    // trims trailing zeros, and "…00.12Z" would sort AFTER "…00.123456Z"
    // because "Z" > any digit). On identical timestamps (same transaction)
    // the article reads first, then the side effects it caused.
    const epoch = (item: ThreadItem) => {
      const t = Date.parse(item.at);
      return Number.isNaN(t) ? 0 : t;
    };
    return merged.sort((a, b) => {
      const cmp = epoch(a) - epoch(b);
      if (cmp !== 0) return cmp;
      if (a.kind !== b.kind) return a.kind === "article" ? -1 : 1;
      return 0;
    });
  }, [ticket.articles, ticket.events]);

  // Keep the newest message in view: bottom-scroll on open and when the
  // thread grows.
  const scrollRef = useRef<HTMLDivElement>(null);
  const articleCount = ticket.articles.length;
  useEffect(() => {
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [ticket.id, articleCount]);

  const [copied, setCopied] = useState(false);
  function copyNumber() {
    void navigator.clipboard.writeText(ticket.number).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  }

  return (
    <div className="flex min-w-0 flex-1 flex-col">
      {/* Header */}
      <header className="flex shrink-0 items-center gap-2.5 border-b px-4 py-2.5">
        <button
          type="button"
          onClick={copyNumber}
          title="Copy ticket number"
          className="flex shrink-0 items-center gap-1 rounded font-mono text-xs text-muted-foreground transition-colors hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          {ticket.number}
          {copied ? (
            <Check className="size-3 text-emerald-500" aria-hidden="true" />
          ) : (
            <Copy className="size-3" aria-hidden="true" />
          )}
        </button>
        <h1 className="min-w-0 flex-1 truncate text-sm font-semibold">
          {ticket.subject}
        </h1>
        <StatusBadge status={ticket.status} className="shrink-0" />
        <PriorityBadge priority={ticket.priority} className="shrink-0" />
        <Button
          variant="ghost"
          size="icon-xs"
          onClick={onToggleSidebar}
          aria-label={
            sidebarCollapsed ? "Show context sidebar" : "Hide context sidebar"
          }
          title={`${sidebarCollapsed ? "Show" : "Hide"} sidebar (x)`}
        >
          <PanelRight aria-hidden="true" />
        </Button>
      </header>

      {/* Thread */}
      <div ref={scrollRef} className="min-h-0 flex-1 overflow-y-auto px-4 py-4">
        <div className="mx-auto flex max-w-3xl flex-col gap-3">
          {items.map((item) =>
            item.kind === "article" ? (
              <ArticleItem key={item.article.id} article={item.article} />
            ) : (
              <EventRow
                key={`event-${item.event.id}`}
                event={item.event}
                names={names}
                teamNames={teamNames}
              />
            ),
          )}
        </div>
      </div>

      {/* Docked composer. Keyed by ticket so navigating the queue remounts
          it with fresh state — a half-written draft (or note mode, or
          pending attachments) must never carry over and get sent to a
          different ticket. */}
      <Composer key={ticket.id} ticket={ticket} />
    </div>
  );
}

function ArticleItem({ article }: { article: Article }) {
  const isAgentSide =
    article.sender_type === "agent" || article.sender_type === "system";
  const authorName =
    article.author?.name ||
    article.author?.email ||
    (article.sender_type === "system" ? "System" : "Customer");

  const avatar = (
    <Avatar className="size-6 shrink-0">
      <AvatarFallback className="text-[10px]">
        {article.author
          ? initials(article.author.name, article.author.email)
          : article.sender_type === "system"
            ? "SD"
            : "?"}
      </AvatarFallback>
    </Avatar>
  );

  return (
    <div
      className={cn(
        "flex items-end gap-2",
        isAgentSide ? "flex-row-reverse" : "flex-row",
      )}
    >
      {avatar}
      <div
        className={cn(
          "min-w-0 max-w-[85%] rounded-lg border px-3 py-2",
          article.is_internal
            ? // Internal notes: distinct amber-tinted surface, both themes.
              "border-amber-500/40 bg-amber-500/10 dark:border-amber-400/30 dark:bg-amber-400/10"
            : isAgentSide
              ? "bg-card"
              : "bg-muted/50",
        )}
      >
        <div className="mb-1 flex items-baseline gap-2 text-xs">
          <span className="font-medium">{authorName}</span>
          {article.is_internal && (
            <span className="rounded-sm border border-amber-500/50 bg-amber-500/15 px-1 font-medium tracking-wide text-amber-700 uppercase dark:border-amber-400/40 dark:text-amber-300">
              Internal
            </span>
          )}
          {article.channel !== "web" && (
            <span className="text-muted-foreground">via {article.channel}</span>
          )}
          <span
            className="ml-auto pl-3 text-muted-foreground"
            title={formatFullTime(article.created_at)}
          >
            {formatThreadTime(article.created_at)}
          </span>
        </div>

        {article.body_html ? (
          <div
            // Sanitized server-side (bluemonday) at write time, per contract.
            dangerouslySetInnerHTML={{ __html: article.body_html }}
            className="text-sm break-words [&_a]:text-primary [&_a]:underline [&_blockquote]:border-l-2 [&_blockquote]:pl-2 [&_blockquote]:text-muted-foreground"
          />
        ) : (
          <p className="text-sm break-words whitespace-pre-wrap">
            {article.body_text}
          </p>
        )}

        {article.attachments.length > 0 && (
          <div className="mt-2 flex flex-wrap gap-1.5">
            {article.attachments.map((attachment) => (
              <a
                key={attachment.id}
                href={attachmentDownloadUrl(attachment.id)}
                download={attachment.filename}
                className="inline-flex max-w-56 items-center gap-1.5 rounded-md border bg-background/60 px-2 py-1 text-xs transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              >
                <Paperclip
                  className="size-3 shrink-0 text-muted-foreground"
                  aria-hidden="true"
                />
                <span className="truncate">{attachment.filename}</span>
                <span className="shrink-0 text-muted-foreground">
                  {formatBytes(attachment.size_bytes)}
                </span>
              </a>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

function payloadString(
  payload: Record<string, unknown>,
  key: string,
): string | null {
  const value = payload[key];
  return typeof value === "string" ? value : null;
}

function describeEvent(
  event: TicketEvent,
  names: Map<string, string>,
  teamNames: Map<string, string>,
): string {
  const actor = event.actor_id
    ? (names.get(event.actor_id) ?? "Someone")
    : "System";
  const from = payloadString(event.payload, "from");
  const to = payloadString(event.payload, "to");

  switch (event.type) {
    case "status_changed": {
      const label = (v: string | null) =>
        v && v in STATUS_LABELS
          ? STATUS_LABELS[v as keyof typeof STATUS_LABELS]
          : v;
      return from && to
        ? `${actor} set status: ${label(from)} → ${label(to)}`
        : `${actor} changed the status`;
    }
    case "priority_changed": {
      const label = (v: string | null) =>
        v && v in PRIORITY_LABELS
          ? PRIORITY_LABELS[v as keyof typeof PRIORITY_LABELS]
          : v;
      return from && to
        ? `${actor} set priority: ${label(from)} → ${label(to)}`
        : `${actor} changed the priority`;
    }
    case "assignee_changed": {
      if (to) return `${actor} assigned ${names.get(to) ?? "an agent"}`;
      if (from) return `${actor} unassigned the ticket`;
      return `${actor} changed the assignee`;
    }
    case "team_changed": {
      if (to) return `${actor} moved to ${teamNames.get(to) ?? "a team"}`;
      if (from) return `${actor} removed the team`;
      return `${actor} changed the team`;
    }
    case "tags_changed":
      return `${actor} updated the tags`;
    default:
      return `${actor} ${event.type.replaceAll("_", " ")}`;
  }
}

/** System events as inline hairline separators. */
function EventRow({
  event,
  names,
  teamNames,
}: {
  event: TicketEvent;
  names: Map<string, string>;
  teamNames: Map<string, string>;
}) {
  return (
    <div className="flex items-center gap-3 py-0.5" role="listitem">
      <span className="h-px flex-1 bg-border" aria-hidden="true" />
      <span className="max-w-[70%] truncate text-xs text-muted-foreground">
        {describeEvent(event, names, teamNames)}
        {/* No extra opacity: muted-foreground at 12px is already near the
            WCAG AA floor; dimming it further fails contrast. */}
        <span className="ml-1.5" title={formatFullTime(event.created_at)}>
          {formatThreadTime(event.created_at)}
        </span>
      </span>
      <span className="h-px flex-1 bg-border" aria-hidden="true" />
    </div>
  );
}
