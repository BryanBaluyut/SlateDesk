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

// Ticket core (M2).
export type TicketStatus = components["schemas"]["TicketStatus"];
export type TicketPriority = components["schemas"]["TicketPriority"];
export type TicketView = components["schemas"]["TicketView"];
export type UserSummary = components["schemas"]["UserSummary"];
export type Ticket = components["schemas"]["Ticket"];
export type TicketList = components["schemas"]["TicketList"];
export type TicketDetail = components["schemas"]["TicketDetail"];
export type Article = components["schemas"]["Article"];
export type ArticleSenderType = components["schemas"]["ArticleSenderType"];
export type Attachment = components["schemas"]["Attachment"];
export type TicketEvent = components["schemas"]["TicketEvent"];
export type Tag = components["schemas"]["Tag"];
export type DashboardCounters = components["schemas"]["DashboardCounters"];
export type StreamEvent = components["schemas"]["StreamEvent"];
export type CreateTicketRequest = components["schemas"]["CreateTicketRequest"];
export type UpdateTicketRequest = components["schemas"]["UpdateTicketRequest"];
export type SetTicketTagsRequest =
  components["schemas"]["SetTicketTagsRequest"];
export type CreateArticleRequest =
  components["schemas"]["CreateArticleRequest"];
export type CreateTagRequest = components["schemas"]["CreateTagRequest"];
export type UpdateTagRequest = components["schemas"]["UpdateTagRequest"];

// Email channel (M3).
export type ArticleChannel = components["schemas"]["ArticleChannel"];
export type DeliveryStatus = components["schemas"]["DeliveryStatus"];
export type Mailbox = components["schemas"]["Mailbox"];
export type MailboxAuthKind = components["schemas"]["MailboxAuthKind"];
export type MailTLSMode = components["schemas"]["MailTLSMode"];
export type CreateMailboxRequest =
  components["schemas"]["CreateMailboxRequest"];
export type UpdateMailboxRequest =
  components["schemas"]["UpdateMailboxRequest"];
export type MailboxTestResult = components["schemas"]["MailboxTestResult"];
export type MailboxTestSendRequest =
  components["schemas"]["MailboxTestSendRequest"];
export type GoogleOauthStart = components["schemas"]["GoogleOauthStart"];
export type ExternalUrlSetting = components["schemas"]["ExternalUrlSetting"];
export type SetExternalUrlRequest =
  components["schemas"]["SetExternalUrlRequest"];

export const ROLES: readonly Role[] = ["customer", "agent", "admin"] as const;

export const TICKET_STATUSES: readonly TicketStatus[] = [
  "open",
  "waiting_on_customer",
  "on_hold",
  "closed",
] as const;

export const TICKET_PRIORITIES: readonly TicketPriority[] = [
  "low",
  "medium",
  "high",
  "critical",
] as const;

export const TICKET_VIEWS: readonly TicketView[] = [
  "my",
  "unassigned",
  "open",
  "closed",
] as const;

export const STATUS_LABELS: Record<TicketStatus, string> = {
  open: "Open",
  waiting_on_customer: "Waiting on customer",
  on_hold: "On hold",
  closed: "Closed",
};

export const PRIORITY_LABELS: Record<TicketPriority, string> = {
  low: "Low",
  medium: "Medium",
  high: "High",
  critical: "Critical",
};

export const VIEW_LABELS: Record<TicketView, string> = {
  my: "My tickets",
  unassigned: "Unassigned",
  open: "All open",
  closed: "Closed",
};

export const MAILBOX_AUTH_KIND_LABELS: Record<MailboxAuthKind, string> = {
  basic: "IMAP/SMTP password",
  oauth_m365: "Microsoft 365",
  oauth_google: "Google",
};

export const TLS_MODES: readonly MailTLSMode[] = [
  "tls",
  "starttls",
  "none",
] as const;

export const TLS_MODE_LABELS: Record<MailTLSMode, string> = {
  tls: "TLS",
  starttls: "STARTTLS",
  none: "None (dev only)",
};
