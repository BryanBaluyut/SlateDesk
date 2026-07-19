import { queryOptions } from "@tanstack/react-query";

import { api } from "./client";
import type {
  PortalArticle,
  PortalCreateTicketRequest,
  PortalReplyRequest,
  PortalTicket,
  PortalTicketDetail,
} from "./types";

/** Customer portal: every call is scoped server-side to the caller's tickets. */
export function listPortalTickets(): Promise<PortalTicket[]> {
  return api.get<PortalTicket[]>("/portal/tickets");
}

export const portalTicketsQueryOptions = queryOptions({
  queryKey: ["portal", "tickets"],
  queryFn: listPortalTickets,
});

export function getPortalTicket(id: string): Promise<PortalTicketDetail> {
  return api.get<PortalTicketDetail>(`/portal/tickets/${id}`);
}

export function portalTicketQueryOptions(id: string) {
  return queryOptions({
    queryKey: ["portal", "tickets", id],
    queryFn: () => getPortalTicket(id),
  });
}

export function createPortalTicket(
  body: PortalCreateTicketRequest,
): Promise<PortalTicketDetail> {
  return api.post<PortalTicketDetail>("/portal/tickets", body);
}

export function replyPortalTicket(
  id: string,
  body: PortalReplyRequest,
): Promise<PortalArticle> {
  return api.post<PortalArticle>(`/portal/tickets/${id}/reply`, body);
}
