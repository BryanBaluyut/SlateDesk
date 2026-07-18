import type { Mailbox } from "@/api/types";
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { formatFullTime, formatRelativeTime } from "@/lib/time";
import { cn } from "@/lib/utils";

/** A poll inside this window counts as "recent" — the poller runs on IMAP
 * IDLE with a 60s fallback, so 10 minutes of silence means trouble. */
const RECENT_POLL_MS = 10 * 60_000;

type Health = "inactive" | "error" | "healthy" | "waiting";

function healthOf(mailbox: Mailbox): Health {
  if (!mailbox.active) return "inactive";
  if (mailbox.last_error !== null) return "error";
  if (
    mailbox.last_poll_at !== null &&
    Date.now() - new Date(mailbox.last_poll_at).getTime() < RECENT_POLL_MS
  ) {
    return "healthy";
  }
  return "waiting";
}

const PILL: Record<Health, { label: string; dot: string; text: string }> = {
  healthy: {
    label: "Healthy",
    dot: "bg-emerald-500",
    text: "text-foreground/80",
  },
  error: {
    label: "Error",
    dot: "bg-red-500",
    text: "text-red-600 dark:text-red-400",
  },
  inactive: {
    label: "Inactive",
    dot: "bg-slate-300 dark:bg-slate-600",
    text: "text-muted-foreground",
  },
  waiting: {
    label: "Waiting",
    dot: "bg-slate-400 dark:bg-slate-500",
    text: "text-muted-foreground",
  },
};

function tooltipText(mailbox: Mailbox, health: Health): string {
  switch (health) {
    case "inactive":
      return "Not polled and not used for sending.";
    case "error": {
      const when = mailbox.last_error_at
        ? ` (${formatRelativeTime(mailbox.last_error_at)} ago)`
        : "";
      const lastGood = mailbox.last_poll_at
        ? ` Last successful poll ${formatRelativeTime(mailbox.last_poll_at)} ago.`
        : " Never polled successfully.";
      return `${mailbox.last_error ?? "Unknown error"}${when}.${lastGood}`;
    }
    case "healthy":
      return mailbox.last_poll_at
        ? `Last polled ${formatRelativeTime(mailbox.last_poll_at)} ago (${formatFullTime(mailbox.last_poll_at)}).`
        : "Polling.";
    case "waiting":
      return mailbox.last_poll_at
        ? `No poll in a while — last success ${formatRelativeTime(mailbox.last_poll_at)} ago.`
        : "No successful poll yet — the poller adopts new mailboxes within a minute.";
  }
}

/**
 * Mailbox health at a glance: green = recently polled and error-free,
 * red = last operation failed (tooltip carries the error + when),
 * grey = inactive, or active but not recently polled.
 */
export function MailboxHealthPill({ mailbox }: { mailbox: Mailbox }) {
  const health = healthOf(mailbox);
  const pill = PILL[health];
  return (
    <TooltipProvider>
      <Tooltip>
        <TooltipTrigger asChild>
          <span
            className={cn(
              "inline-flex cursor-default items-center gap-1.5 text-xs whitespace-nowrap",
              pill.text,
            )}
          >
            <span
              className={cn("size-1.5 shrink-0 rounded-full", pill.dot)}
              aria-hidden="true"
            />
            {pill.label}
          </span>
        </TooltipTrigger>
        <TooltipContent className="max-w-72">
          {tooltipText(mailbox, health)}
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  );
}
