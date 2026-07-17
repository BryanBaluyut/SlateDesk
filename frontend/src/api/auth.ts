import { queryOptions } from "@tanstack/react-query";

import { api } from "./client";
import type { LoginRequest, User } from "./types";

/**
 * POST /auth/login returns 204; the HttpOnly `sd_session` cookie is the
 * session. Callers must refetch /auth/me afterwards to learn who logged in.
 */
export function login(body: LoginRequest): Promise<void> {
  return api.post<void>("/auth/login", body);
}

export function logout(): Promise<void> {
  return api.post<void>("/auth/logout");
}

export function getMe(): Promise<User> {
  return api.get<User>("/auth/me");
}

export const meQueryOptions = queryOptions({
  queryKey: ["auth", "me"],
  queryFn: getMe,
  staleTime: 60_000,
  retry: false,
});
