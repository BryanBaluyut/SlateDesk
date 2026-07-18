import {
  queryOptions,
  useMutation,
  useQueryClient,
} from "@tanstack/react-query";

import { api } from "./client";
import type {
  Article,
  CreateArticleRequest,
  CreateTicketRequest,
  Tag,
  Ticket,
  TicketDetail,
  TicketList,
  TicketPriority,
  TicketStatus,
  TicketView,
  UpdateTicketRequest,
} from "./types";

/** Queue page size. Plain list — the API caps limit at 200. */
export const TICKETS_PAGE_SIZE = 100;

/** Filters accepted by GET /tickets, mirrored in the /tickets URL params. */
export interface TicketFilters {
  view?: TicketView;
  status?: TicketStatus[];
  priority?: TicketPriority[];
  tag_id?: string;
  /** Only tickets closed since midnight (the dashboard tile's predicate). */
  closed_today?: boolean;
  q?: string;
}

function ticketsQueryString(filters: TicketFilters): string {
  const params = new URLSearchParams();
  if (filters.view) params.set("view", filters.view);
  for (const s of filters.status ?? []) params.append("status", s);
  for (const p of filters.priority ?? []) params.append("priority", p);
  if (filters.tag_id) params.set("tag_id", filters.tag_id);
  if (filters.closed_today) params.set("closed_today", "true");
  if (filters.q) params.set("q", filters.q);
  params.set("limit", String(TICKETS_PAGE_SIZE));
  return params.toString();
}

export function ticketsQueryOptions(filters: TicketFilters) {
  return queryOptions({
    queryKey: ["tickets", "list", filters] as const,
    queryFn: () =>
      api.get<TicketList>(`/tickets?${ticketsQueryString(filters)}`),
  });
}

export function ticketQueryOptions(id: string) {
  return queryOptions({
    queryKey: ["tickets", "detail", id] as const,
    queryFn: () => api.get<TicketDetail>(`/tickets/${id}`),
  });
}

/** Invalidate everything a ticket change can affect. */
function invalidateTicketData(
  queryClient: ReturnType<typeof useQueryClient>,
  ticketId?: string,
) {
  void queryClient.invalidateQueries({ queryKey: ["tickets", "list"] });
  void queryClient.invalidateQueries({ queryKey: ["dashboard"] });
  if (ticketId) {
    void queryClient.invalidateQueries({
      queryKey: ["tickets", "detail", ticketId],
    });
  }
}

export function useCreateTicket() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: CreateTicketRequest) =>
      api.post<TicketDetail>("/tickets", body),
    onSuccess: (ticket) => {
      queryClient.setQueryData(["tickets", "detail", ticket.id], ticket);
      invalidateTicketData(queryClient);
    },
  });
}

/**
 * PATCH /tickets/{id} with an optimistic detail-cache patch. Callers pass the
 * request body plus the already-resolved optimistic fields (e.g. the assignee
 * summary matching assignee_id) so the thread and sidebar update instantly.
 */
export function useUpdateTicket(id: string) {
  const queryClient = useQueryClient();
  const detailKey = ["tickets", "detail", id] as const;
  return useMutation({
    mutationFn: ({ body }: { body: UpdateTicketRequest; optimistic?: Partial<Ticket> }) =>
      api.patch<Ticket>(`/tickets/${id}`, body),
    onMutate: async ({ optimistic }) => {
      await queryClient.cancelQueries({ queryKey: detailKey });
      const previous = queryClient.getQueryData<TicketDetail>(detailKey);
      if (previous && optimistic) {
        queryClient.setQueryData<TicketDetail>(detailKey, {
          ...previous,
          ...optimistic,
        });
      }
      return { previous };
    },
    onError: (_err, _vars, context) => {
      if (context?.previous) {
        queryClient.setQueryData(detailKey, context.previous);
      }
    },
    onSettled: () => invalidateTicketData(queryClient, id),
  });
}

/** PUT /tickets/{id}/tags (full replacement) with optimistic tags. */
export function useSetTicketTags(id: string) {
  const queryClient = useQueryClient();
  const detailKey = ["tickets", "detail", id] as const;
  return useMutation({
    mutationFn: ({ tagIds }: { tagIds: string[]; optimisticTags?: Tag[] }) =>
      api.put<Tag[]>(`/tickets/${id}/tags`, { tag_ids: tagIds }),
    onMutate: async ({ optimisticTags }) => {
      await queryClient.cancelQueries({ queryKey: detailKey });
      const previous = queryClient.getQueryData<TicketDetail>(detailKey);
      if (previous && optimisticTags) {
        queryClient.setQueryData<TicketDetail>(detailKey, {
          ...previous,
          tags: optimisticTags,
        });
      }
      return { previous };
    },
    onError: (_err, _vars, context) => {
      if (context?.previous) {
        queryClient.setQueryData(detailKey, context.previous);
      }
    },
    onSettled: () => invalidateTicketData(queryClient, id),
  });
}

/**
 * POST /tickets/{id}/articles. No optimistic insert — the composer stays
 * disabled during the round-trip and attachments upload afterwards; callers
 * invalidate once uploads finish (see the composer).
 */
export function useCreateArticle(ticketId: string) {
  return useMutation({
    mutationFn: (body: CreateArticleRequest) =>
      api.post<Article>(`/tickets/${ticketId}/articles`, body),
  });
}

/**
 * POST /articles/{id}/retry-send — re-enqueue a `failed` outbound email
 * delivery. The 202 body is the article back in `queued`; patch it into
 * the detail cache for instant badge feedback (later transitions arrive
 * as SSE `article.updated` invalidations).
 */
export function useRetryArticleSend(ticketId: string) {
  const queryClient = useQueryClient();
  const detailKey = ["tickets", "detail", ticketId] as const;
  return useMutation({
    mutationFn: (articleId: string) =>
      api.post<Article>(`/articles/${articleId}/retry-send`),
    onSuccess: (article) => {
      queryClient.setQueryData<TicketDetail>(detailKey, (previous) =>
        previous
          ? {
              ...previous,
              articles: previous.articles.map((a) =>
                a.id === article.id ? article : a,
              ),
            }
          : previous,
      );
    },
    onError: () => {
      // 409 = no longer `failed` (a concurrent retry, or the badge was
      // stale). The server holds the truth — refetch the thread.
      void queryClient.invalidateQueries({ queryKey: detailKey });
    },
  });
}
