import {
  queryOptions,
  useMutation,
  useQueryClient,
} from "@tanstack/react-query";

import { api } from "./client";
import type {
  CreateMailboxRequest,
  ExternalUrlSetting,
  GoogleOauthStart,
  Mailbox,
  MailboxTestResult,
  UpdateMailboxRequest,
} from "./types";

/**
 * Mailbox admin surface (M3). Everything here is admin-only server-side;
 * the /settings route group applies the matching client-side gate.
 *
 * The list refetches on an interval while mounted: mailbox health
 * (last_poll_at / last_error) changes from the poller, not from user
 * actions, and there is no mailbox-scoped SSE event to invalidate on.
 */
export const mailboxesQueryOptions = queryOptions({
  queryKey: ["mailboxes"],
  queryFn: () => api.get<Mailbox[]>("/mailboxes"),
  refetchInterval: 30_000,
});

export function useCreateMailbox() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: CreateMailboxRequest) =>
      api.post<Mailbox>("/mailboxes", body),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: ["mailboxes"] }),
  });
}

export function useUpdateMailbox() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ id, body }: { id: string; body: UpdateMailboxRequest }) =>
      api.patch<Mailbox>(`/mailboxes/${id}`, body),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: ["mailboxes"] }),
  });
}

export function useDeleteMailbox() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.delete<void>(`/mailboxes/${id}`),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: ["mailboxes"] }),
  });
}

/**
 * Live connectivity tests. A failed test is still HTTP 200 with `ok=false`
 * (the test ran; the result is the payload), so callers branch on `ok`,
 * not on thrown ApiError. May take several seconds — network timeouts.
 */
export function testMailboxFetch(id: string): Promise<MailboxTestResult> {
  return api.post<MailboxTestResult>(`/mailboxes/${id}/test-fetch`);
}

export function testMailboxSend(
  id: string,
  to: string,
): Promise<MailboxTestResult> {
  return api.post<MailboxTestResult>(`/mailboxes/${id}/test-send`, { to });
}

/**
 * Google connect flow: the server mints the authorization URL (offline
 * access + signed single-use state); the browser opens it in a new window.
 * The callback redirects that window to /settings/mailboxes?connected=1.
 * 409 = mailbox is not oauth_google, or the external URL is unset.
 */
export function startGoogleOauth(id: string): Promise<GoogleOauthStart> {
  return api.post<GoogleOauthStart>(`/mailboxes/${id}/oauth/google/start`);
}

export const externalUrlQueryOptions = queryOptions({
  queryKey: ["settings", "external-url"],
  queryFn: () => api.get<ExternalUrlSetting>("/settings/external-url"),
});

export function useSetExternalUrl() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (externalUrl: string) =>
      api.put<ExternalUrlSetting>("/settings/external-url", {
        external_url: externalUrl,
      }),
    onSuccess: (setting) =>
      queryClient.setQueryData(["settings", "external-url"], setting),
  });
}
