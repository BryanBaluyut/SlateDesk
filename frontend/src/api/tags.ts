import {
  queryOptions,
  useMutation,
  useQueryClient,
} from "@tanstack/react-query";

import { api } from "./client";
import type { CreateTagRequest, Tag } from "./types";

export const tagsQueryOptions = queryOptions({
  queryKey: ["tags"],
  queryFn: () => api.get<Tag[]>("/tags"),
});

export function useCreateTag() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: CreateTagRequest) => api.post<Tag>("/tags", body),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["tags"] }),
  });
}
