import { queryOptions } from "@tanstack/react-query";

import { api } from "./client";
import type {
  InstanceSettings,
  SetupAdminRequest,
  SetupCompleteResult,
  SetupInstanceRequest,
  SetupStatus,
  User,
} from "./types";

/** First-run installer endpoints (unauthenticated door; token-gated admin). */
export function getSetupStatus(): Promise<SetupStatus> {
  return api.get<SetupStatus>("/setup/status");
}

export const setupStatusQueryOptions = queryOptions({
  queryKey: ["setup", "status"],
  queryFn: getSetupStatus,
  retry: false,
  staleTime: 0,
});

export function createSetupAdmin(body: SetupAdminRequest): Promise<User> {
  return api.post<User>("/setup/admin", body);
}

export function setSetupInstance(
  body: SetupInstanceRequest,
): Promise<InstanceSettings> {
  return api.post<InstanceSettings>("/setup/instance", body);
}

export function completeSetup(): Promise<SetupCompleteResult> {
  return api.post<SetupCompleteResult>("/setup/complete");
}
