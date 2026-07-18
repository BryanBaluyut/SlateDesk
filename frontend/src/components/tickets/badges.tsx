import type {
  Tag,
  TicketPriority,
  TicketStatus,
} from "@/api/types";
import { PRIORITY_LABELS, STATUS_LABELS } from "@/api/types";
import { cn } from "@/lib/utils";

/**
 * Calm status/priority markers: a colored dot plus quiet text, not loud
 * pills. Amber is reserved for internal notes, so waiting_on_customer uses
 * violet instead.
 */

const STATUS_DOT: Record<TicketStatus, string> = {
  open: "bg-emerald-500",
  waiting_on_customer: "bg-violet-500",
  on_hold: "bg-slate-400 dark:bg-slate-500",
  closed: "bg-slate-300 dark:bg-slate-600",
};

export function StatusBadge({
  status,
  className,
  short = false,
}: {
  status: TicketStatus;
  className?: string;
  /** Compact label for queue rows ("Waiting" instead of the full phrase). */
  short?: boolean;
}) {
  const label =
    short && status === "waiting_on_customer"
      ? "Waiting"
      : STATUS_LABELS[status];
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 text-xs whitespace-nowrap",
        status === "closed" ? "text-muted-foreground" : "text-foreground/80",
        className,
      )}
    >
      <span
        className={cn("size-1.5 shrink-0 rounded-full", STATUS_DOT[status])}
        aria-hidden="true"
      />
      {label}
    </span>
  );
}

const PRIORITY_STYLE: Record<TicketPriority, string> = {
  low: "text-muted-foreground",
  medium: "text-muted-foreground",
  high: "text-orange-600 dark:text-orange-400",
  critical: "text-red-600 dark:text-red-400",
};

/** Linear-style priority glyph: three bars, filled by severity. */
export function PriorityIcon({
  priority,
  className,
}: {
  priority: TicketPriority;
  className?: string;
}) {
  const filled = { low: 1, medium: 2, high: 3, critical: 3 }[priority];
  return (
    <span
      className={cn(
        "inline-flex items-end gap-px",
        PRIORITY_STYLE[priority],
        className,
      )}
      aria-hidden="true"
    >
      {[4, 7, 10].map((h, i) => (
        <span
          key={h}
          style={{ height: h }}
          className={cn(
            "w-[3px] rounded-[1px]",
            i < filled ? "bg-current" : "bg-current opacity-25",
          )}
        />
      ))}
    </span>
  );
}

export function PriorityBadge({
  priority,
  className,
}: {
  priority: TicketPriority;
  className?: string;
}) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 text-xs whitespace-nowrap",
        PRIORITY_STYLE[priority],
        className,
      )}
      title={`Priority: ${PRIORITY_LABELS[priority]}`}
    >
      <PriorityIcon priority={priority} />
      {PRIORITY_LABELS[priority]}
    </span>
  );
}

/** Small tag chip, tinted by the tag's color when set. */
export function TagChip({
  tag,
  className,
  onClick,
  active,
}: {
  tag: Tag;
  className?: string;
  onClick?: () => void;
  active?: boolean;
}) {
  const dot = (
    <span
      className="size-1.5 shrink-0 rounded-full"
      style={{ backgroundColor: tag.color ?? "var(--muted-foreground)" }}
      aria-hidden="true"
    />
  );
  const base = cn(
    "inline-flex max-w-40 items-center gap-1.5 rounded-full border px-2 py-0.5 text-xs",
    active
      ? "border-primary/40 bg-primary/10 text-foreground"
      : "text-muted-foreground",
    onClick &&
      "cursor-pointer transition-colors hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
    className,
  );
  if (onClick) {
    return (
      <button type="button" onClick={onClick} className={base} aria-pressed={active}>
        {dot}
        <span className="truncate">{tag.name}</span>
      </button>
    );
  }
  return (
    <span className={base}>
      {dot}
      <span className="truncate">{tag.name}</span>
    </span>
  );
}
