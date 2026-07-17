import {
  queryOptions,
  useMutation,
  useQueryClient,
} from "@tanstack/react-query";

import { api } from "./client";
import type {
  CreateTeamRequest,
  Team,
  TeamWithMembers,
  UpdateTeamRequest,
} from "./types";

export const teamsQueryOptions = queryOptions({
  queryKey: ["teams"],
  queryFn: () => api.get<Team[]>("/teams"),
});

/** Team detail includes members; the list endpoint does not. */
export function teamQueryOptions(id: string) {
  return queryOptions({
    queryKey: ["teams", id],
    queryFn: () => api.get<TeamWithMembers>(`/teams/${id}`),
  });
}

function setTeamMembers(id: string, userIds: string[]) {
  return api.put<TeamWithMembers>(`/teams/${id}/members`, {
    user_ids: userIds,
  });
}

/** Creates the team, then replaces its membership when memberIds is given. */
export function useCreateTeam() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({
      body,
      memberIds,
    }: {
      body: CreateTeamRequest;
      memberIds: string[];
    }) => {
      const team = await api.post<Team>("/teams", body);
      if (memberIds.length > 0) {
        await setTeamMembers(team.id, memberIds);
      }
      return team;
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["teams"] }),
  });
}

/** Patches name/description, then replaces the membership list (PUT). */
export function useUpdateTeam() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({
      id,
      body,
      memberIds,
    }: {
      id: string;
      body: UpdateTeamRequest;
      memberIds: string[];
    }) => {
      const team = await api.patch<Team>(`/teams/${id}`, body);
      await setTeamMembers(id, memberIds);
      return team;
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["teams"] }),
  });
}

export function useDeleteTeam() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.delete<void>(`/teams/${id}`),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["teams"] }),
  });
}
