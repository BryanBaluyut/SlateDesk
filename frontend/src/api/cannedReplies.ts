import {
  queryOptions,
  useMutation,
  useQueryClient,
} from "@tanstack/react-query";

import { api } from "./client";
import type {
  CannedReply,
  CreateCannedReplyRequest,
  UpdateCannedReplyRequest,
} from "./types";

/**
 * Canned-reply surface (M4). Agent + admin server-side. The stored body keeps
 * its raw {{...}} template; substitution happens client-side at insert time
 * (see lib/canned.ts).
 */
export const cannedRepliesQueryOptions = queryOptions({
  queryKey: ["canned-replies"],
  queryFn: () => api.get<CannedReply[]>("/canned-replies"),
});

export function useCreateCannedReply() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: CreateCannedReplyRequest) =>
      api.post<CannedReply>("/canned-replies", body),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: ["canned-replies"] }),
  });
}

export function useUpdateCannedReply() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({
      id,
      body,
    }: {
      id: string;
      body: UpdateCannedReplyRequest;
    }) => api.patch<CannedReply>(`/canned-replies/${id}`, body),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: ["canned-replies"] }),
  });
}

export function useDeleteCannedReply() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.delete<void>(`/canned-replies/${id}`),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: ["canned-replies"] }),
  });
}
