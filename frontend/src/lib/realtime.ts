import { useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";

import type { StreamEvent } from "@/api/types";

/** Close the stream once the tab has been hidden this long; resume + refetch
 * on visibility. */
const HIDDEN_SUSPEND_MS = 5 * 60_000;
const BACKOFF_BASE_MS = 1_000;
const BACKOFF_MAX_MS = 30_000;

function isStreamEvent(value: unknown): value is StreamEvent {
  if (typeof value !== "object" || value === null) return false;
  const v = value as Record<string, unknown>;
  // "resync" carries no ticket_id: the server may have dropped events
  // (e.g. its LISTEN connection was re-established).
  if (v.type === "resync") return true;
  return (
    (v.type === "ticket.created" ||
      v.type === "ticket.updated" ||
      v.type === "article.created" ||
      // M3: delivery_status transitions on outbound email articles.
      v.type === "article.updated") &&
    typeof v.ticket_id === "string"
  );
}

/**
 * Single EventSource to GET /api/v1/events while the workspace is open
 * (agents/admins only — the caller gates on role). Events are invalidation
 * hints: on {type, ticket_id} we invalidate the ticket lists, that ticket's
 * detail query, and the dashboard counters.
 *
 * - Reconnects with exponential backoff (we manage reconnection ourselves;
 *   the browser's automatic retry is disabled by closing on error).
 * - Suspends when the tab has been hidden >5 minutes; resumes and refetches
 *   everything ticket-shaped on visibility, because events were missed.
 */
export function useRealtimeEvents(enabled: boolean) {
  const queryClient = useQueryClient();

  useEffect(() => {
    if (!enabled) return;

    let source: EventSource | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let suspendTimer: ReturnType<typeof setTimeout> | null = null;
    let attempts = 0;
    let suspended = false;
    let disposed = false;

    const invalidateAll = () => {
      void queryClient.invalidateQueries({ queryKey: ["tickets"] });
      void queryClient.invalidateQueries({ queryKey: ["dashboard"] });
    };

    const onEvent = (raw: string) => {
      let payload: unknown;
      try {
        payload = JSON.parse(raw);
      } catch {
        return; // Not ours; heartbeat comments never reach onmessage anyway.
      }
      if (!isStreamEvent(payload)) return;
      if (payload.type === "resync" || !payload.ticket_id) {
        // The server itself may have missed events: refetch everything.
        invalidateAll();
        return;
      }
      void queryClient.invalidateQueries({ queryKey: ["tickets", "list"] });
      void queryClient.invalidateQueries({
        queryKey: ["tickets", "detail", payload.ticket_id],
      });
      void queryClient.invalidateQueries({ queryKey: ["dashboard"] });
    };

    const close = () => {
      if (source) {
        source.close();
        source = null;
      }
      if (reconnectTimer) {
        clearTimeout(reconnectTimer);
        reconnectTimer = null;
      }
    };

    const open = () => {
      if (disposed || suspended || source) return;
      source = new EventSource("/api/v1/events");
      source.onopen = () => {
        if (attempts > 0) {
          // This open follows a dropped connection: events during the gap
          // are gone (they are hints, not a durable feed), so honor the
          // server contract — refetch everything on reconnect.
          invalidateAll();
        }
        attempts = 0;
      };
      source.onmessage = (e: MessageEvent<string>) => onEvent(e.data);
      source.onerror = () => {
        // Take over reconnection: close and back off ourselves.
        close();
        if (disposed || suspended) return;
        const delay = Math.min(
          BACKOFF_MAX_MS,
          BACKOFF_BASE_MS * 2 ** attempts,
        );
        attempts += 1;
        reconnectTimer = setTimeout(open, delay);
      };
    };

    const onVisibilityChange = () => {
      if (document.visibilityState === "hidden") {
        if (suspendTimer) clearTimeout(suspendTimer);
        suspendTimer = setTimeout(() => {
          suspended = true;
          close();
        }, HIDDEN_SUSPEND_MS);
      } else {
        if (suspendTimer) {
          clearTimeout(suspendTimer);
          suspendTimer = null;
        }
        if (suspended) {
          // We were offline; anything may have changed.
          suspended = false;
          attempts = 0;
          invalidateAll();
          open();
        }
      }
    };

    document.addEventListener("visibilitychange", onVisibilityChange);
    open();

    return () => {
      disposed = true;
      document.removeEventListener("visibilitychange", onVisibilityChange);
      if (suspendTimer) clearTimeout(suspendTimer);
      close();
    };
  }, [enabled, queryClient]);
}
