import {
  queryOptions,
  useMutation,
  useQueryClient,
} from "@tanstack/react-query";

import { api } from "./client";
import type { ApiKey, ApiKeyCreated, CreateApiKeyRequest } from "./types";

/**
 * API-key admin surface (M4). Admin-only server-side; the /settings route
 * group applies the matching client-side gate. The plaintext key is returned
 * only once (on create) and never listed again.
 */
export const apiKeysQueryOptions = queryOptions({
  queryKey: ["api-keys"],
  queryFn: () => api.get<ApiKey[]>("/api-keys"),
});

export function useCreateApiKey() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: CreateApiKeyRequest) =>
      api.post<ApiKeyCreated>("/api-keys", body),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["api-keys"] }),
  });
}

export function useRevokeApiKey() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.delete<void>(`/api-keys/${id}`),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["api-keys"] }),
  });
}
