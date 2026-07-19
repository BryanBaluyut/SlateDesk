import { api } from "./client";
import type { PublicTicketAccepted, PublicTicketRequest } from "./types";

/**
 * Submit the built-in public web form. Unauthenticated; the server rate-limits
 * per IP, honours the `_honeypot` field, and never reveals whether the email
 * already had an account.
 */
export function submitPublicTicket(
  body: PublicTicketRequest,
): Promise<PublicTicketAccepted> {
  return api.post<PublicTicketAccepted>("/public/tickets", body);
}
