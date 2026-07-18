/** Compact relative/absolute time formatting for queue rows and the thread. */

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

const dateFormat = new Intl.DateTimeFormat(undefined, {
  month: "short",
  day: "numeric",
});
const dateYearFormat = new Intl.DateTimeFormat(undefined, {
  month: "short",
  day: "numeric",
  year: "numeric",
});
const timeFormat = new Intl.DateTimeFormat(undefined, {
  hour: "numeric",
  minute: "2-digit",
});
const fullFormat = new Intl.DateTimeFormat(undefined, {
  dateStyle: "medium",
  timeStyle: "short",
});

/** "just now", "5m", "3h", "4d", then "Mar 8" / "Mar 8, 2025". Queue rows. */
export function formatRelativeTime(iso: string): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "";
  const diff = Date.now() - then;
  if (diff < MINUTE) return "just now";
  if (diff < HOUR) return `${Math.floor(diff / MINUTE)}m`;
  if (diff < DAY) return `${Math.floor(diff / HOUR)}h`;
  if (diff < 7 * DAY) return `${Math.floor(diff / DAY)}d`;
  const date = new Date(then);
  return date.getFullYear() === new Date().getFullYear()
    ? dateFormat.format(date)
    : dateYearFormat.format(date);
}

/** "3:42 PM" today, else "Mar 8, 3:42 PM"-ish. Thread timestamps. */
export function formatThreadTime(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  const now = new Date();
  if (date.toDateString() === now.toDateString()) {
    return timeFormat.format(date);
  }
  return `${
    date.getFullYear() === now.getFullYear()
      ? dateFormat.format(date)
      : dateYearFormat.format(date)
  }, ${timeFormat.format(date)}`;
}

/** Full timestamp for title attributes. */
export function formatFullTime(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;
  return fullFormat.format(date);
}

/** "12.3 KB", "4.2 MB" — attachment chips. */
export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}
