import { CircleAlert } from "lucide-react";

import { isApiError } from "@/api/client";

/**
 * Renders an RFC 9457 problem (or any error) as a calm inline alert.
 * Prefers the problem `detail`, falls back to `title`, then to a generic
 * message — never raw stack traces.
 */
export function ProblemAlert({ error }: { error: unknown }) {
  let message = "Something went wrong. Please try again.";
  if (isApiError(error)) {
    message = error.problem?.detail || error.problem?.title || message;
  }
  return (
    <div
      role="alert"
      className="flex items-start gap-2 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive"
    >
      <CircleAlert className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
      <span>{message}</span>
    </div>
  );
}
