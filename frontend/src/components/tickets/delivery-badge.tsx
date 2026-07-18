import { Check, RotateCw } from "lucide-react";

import { useRetryArticleSend } from "@/api/tickets";
import { isApiError } from "@/api/client";
import type { Article } from "@/api/types";
import { cn } from "@/lib/utils";

/**
 * Outbound email delivery badge, driven by `article.delivery_status`
 * (null = not an outbound email, render nothing). queued/sending are
 * subtle in-progress states, sent is a quiet check — only `failed` is
 * loud: a red badge with the retry action right next to it. Transitions
 * stream in live via SSE `article.updated` invalidations.
 */
export function DeliveryBadge({ article }: { article: Article }) {
  const retry = useRetryArticleSend(article.ticket_id);
  const status = article.delivery_status;
  if (status === null) return null;

  switch (status) {
    case "queued":
    case "sending":
      return (
        <span
          className="inline-flex items-center gap-1 text-muted-foreground"
          title={
            status === "queued"
              ? "Email queued for delivery"
              : "Handing the email to the mail server"
          }
        >
          <span
            className="size-1.5 shrink-0 animate-pulse rounded-full bg-current opacity-60"
            aria-hidden="true"
          />
          {status === "queued" ? "Queued" : "Sending…"}
        </span>
      );
    case "sent":
      return (
        <span
          className="inline-flex items-center gap-1 text-muted-foreground"
          title="Email accepted by the mail server"
        >
          <Check className="size-3 shrink-0" aria-hidden="true" />
          Sent
        </span>
      );
    case "failed":
      return (
        <span className="inline-flex items-center gap-1.5">
          <span
            className="rounded-sm border border-red-500/50 bg-red-500/10 px-1 font-medium tracking-wide text-red-700 uppercase dark:border-red-400/40 dark:text-red-300"
            title="Every delivery attempt failed"
          >
            Send failed
          </span>
          <button
            type="button"
            onClick={() => retry.mutate(article.id)}
            disabled={retry.isPending}
            className={cn(
              "inline-flex items-center gap-1 rounded text-muted-foreground underline-offset-2 transition-colors",
              "hover:text-foreground hover:underline",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
              "disabled:pointer-events-none disabled:opacity-50",
            )}
          >
            <RotateCw
              className={cn("size-3 shrink-0", retry.isPending && "animate-spin")}
              aria-hidden="true"
            />
            {retry.isPending ? "Retrying…" : "Retry send"}
          </button>
          {retry.isError && (
            <span className="text-red-600 dark:text-red-400">
              {isApiError(retry.error)
                ? retry.error.problem?.detail || "Retry failed"
                : "Retry failed"}
            </span>
          )}
        </span>
      );
  }
}
