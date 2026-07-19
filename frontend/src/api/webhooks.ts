import {
  queryOptions,
  useMutation,
  useQueryClient,
} from "@tanstack/react-query";

import { api } from "./client";
import type {
  CreateWebhookRequest,
  UpdateWebhookRequest,
  Webhook,
  WebhookCreated,
  WebhookDelivery,
  WebhookTestResult,
} from "./types";

/**
 * Webhook admin surface (M4). Admin-only server-side; the /settings route
 * group applies the matching client-side gate. The signing secret is returned
 * only once (on create) and never listed again.
 */
export const webhooksQueryOptions = queryOptions({
  queryKey: ["webhooks"],
  queryFn: () => api.get<Webhook[]>("/webhooks"),
});

export function webhookDeliveriesQueryOptions(id: string) {
  return queryOptions({
    queryKey: ["webhooks", id, "deliveries"],
    queryFn: () =>
      api.get<WebhookDelivery[]>(`/webhooks/${id}/deliveries?limit=50`),
    // Deliveries land from the River worker, not user actions; poll while the
    // drawer is open.
    refetchInterval: 5_000,
  });
}

export function useCreateWebhook() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: CreateWebhookRequest) =>
      api.post<WebhookCreated>("/webhooks", body),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["webhooks"] }),
  });
}

export function useUpdateWebhook() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ id, body }: { id: string; body: UpdateWebhookRequest }) =>
      api.patch<Webhook>(`/webhooks/${id}`, body),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["webhooks"] }),
  });
}

export function useDeleteWebhook() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.delete<void>(`/webhooks/${id}`),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["webhooks"] }),
  });
}

/**
 * Synchronous test ping. A failing endpoint is still HTTP 200 with `ok=false`
 * (the ping ran; the result is the payload), so callers branch on `ok`, not on
 * a thrown ApiError.
 */
export function testWebhook(id: string): Promise<WebhookTestResult> {
  return api.post<WebhookTestResult>(`/webhooks/${id}/test`);
}
