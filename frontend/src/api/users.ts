import {
  queryOptions,
  useMutation,
  useQueryClient,
} from "@tanstack/react-query";

import { api } from "./client";
import type { CreateUserRequest, UpdateUserRequest, User } from "./types";

export const usersQueryOptions = queryOptions({
  queryKey: ["users"],
  queryFn: () => api.get<User[]>("/users"),
});

/**
 * Active agents + admins — the assignable population for tickets. Two role
 * filters, one merged list, sorted by name for combobox display.
 */
export const agentsQueryOptions = queryOptions({
  queryKey: ["users", "agents"],
  queryFn: async () => {
    const [agents, admins] = await Promise.all([
      api.get<User[]>("/users?role=agent&active=true&limit=200"),
      api.get<User[]>("/users?role=admin&active=true&limit=200"),
    ]);
    return [...agents, ...admins].sort((a, b) =>
      (a.name || a.email).localeCompare(b.name || b.email),
    );
  },
  staleTime: 5 * 60_000,
});

export function useCreateUser() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: CreateUserRequest) => api.post<User>("/users", body),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["users"] }),
  });
}

export function useUpdateUser() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ id, body }: { id: string; body: UpdateUserRequest }) =>
      api.patch<User>(`/users/${id}`, body),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["users"] }),
  });
}

/** Soft delete: DELETE /users/{id} sets active=false and kills sessions. */
export function useDeactivateUser() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.delete<void>(`/users/${id}`),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["users"] }),
  });
}
