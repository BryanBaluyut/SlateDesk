import type { Ticket } from "@/api/types";

/**
 * Canned-reply variable substitution (M4). The server stores and returns the
 * raw template; substitution is done here, client-side, at composer-insert
 * time — the ticket and its requester are already loaded in the workspace, so
 * no round trip is needed and the stored snippet stays reusable across tickets.
 *
 * Supported placeholders (unknown ones are left verbatim so a typo is visible
 * rather than silently blanked):
 *
 *   {{ticket.number}}      the human ticket number (YYYYMMDD-NNNN)
 *   {{ticket.subject}}     the ticket subject
 *   {{requester.name}}     the requester's display name
 *   {{requester.email}}    the requester's email
 */
export const CANNED_PLACEHOLDERS = [
  "{{ticket.number}}",
  "{{ticket.subject}}",
  "{{requester.name}}",
  "{{requester.email}}",
] as const;

export function renderCanned(template: string, ticket: Ticket): string {
  const values: Record<string, string> = {
    "ticket.number": ticket.number,
    "ticket.subject": ticket.subject,
    "requester.name": ticket.requester.name,
    "requester.email": ticket.requester.email,
  };
  // Match {{ key }} with optional inner whitespace; keep unknown keys as-is.
  return template.replace(/\{\{\s*([\w.]+)\s*\}\}/g, (match, key: string) =>
    key in values ? values[key] : match,
  );
}
