/**
 * Aliases over the generated OpenAPI types (src/api/schema.d.ts, owned by
 * the codegen pipeline). All application code imports from here — never from
 * schema.d.ts directly — so regeneration never ripples through the app.
 */
import type { components } from "./schema";

export type Problem = components["schemas"]["Problem"];
export type Role = components["schemas"]["Role"];
export type User = components["schemas"]["User"];
export type Team = components["schemas"]["Team"];
export type TeamWithMembers = components["schemas"]["TeamWithMembers"];
export type LoginRequest = components["schemas"]["LoginRequest"];
export type CreateUserRequest = components["schemas"]["CreateUserRequest"];
export type UpdateUserRequest = components["schemas"]["UpdateUserRequest"];
export type CreateTeamRequest = components["schemas"]["CreateTeamRequest"];
export type UpdateTeamRequest = components["schemas"]["UpdateTeamRequest"];
export type SetTeamMembersRequest =
  components["schemas"]["SetTeamMembersRequest"];

export const ROLES: readonly Role[] = ["customer", "agent", "admin"] as const;
