/**
 * Small typed wrapper over fetch for the /api/v1 backend.
 *
 * - Sends/receives JSON, cookie-authenticated (HttpOnly session cookie).
 * - Parses RFC 9457 application/problem+json error bodies into ApiError.
 * - 401 redirect handling lives in the router/query layer (see main.tsx and
 *   the _auth route guard), not here, so login-page requests can handle 401
 *   locally.
 */
import type { Problem } from "./types";

const API_BASE = "/api/v1";

export class ApiError extends Error {
  readonly status: number;
  readonly problem: Problem | null;

  constructor(status: number, problem: Problem | null) {
    super(problem?.detail || problem?.title || `Request failed (${status})`);
    this.name = "ApiError";
    this.status = status;
    this.problem = problem;
  }
}

export function isApiError(err: unknown): err is ApiError {
  return err instanceof ApiError;
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const init: RequestInit = {
    method,
    credentials: "same-origin",
    headers: { Accept: "application/json" },
  };
  if (body !== undefined) {
    init.headers = { ...init.headers, "Content-Type": "application/json" };
    init.body = JSON.stringify(body);
  }

  const res = await fetch(`${API_BASE}${path}`, init);
  if (!res.ok) {
    let problem: Problem | null = null;
    const contentType = res.headers.get("Content-Type") ?? "";
    if (contentType.includes("application/problem+json")) {
      problem = (await res.json().catch(() => null)) as Problem | null;
    }
    throw new ApiError(res.status, problem);
  }

  if (res.status === 204) {
    return undefined as T;
  }
  return (await res.json()) as T;
}

export const api = {
  get: <T>(path: string) => request<T>("GET", path),
  post: <T>(path: string, body?: unknown) => request<T>("POST", path, body),
  patch: <T>(path: string, body?: unknown) => request<T>("PATCH", path, body),
  put: <T>(path: string, body?: unknown) => request<T>("PUT", path, body),
  delete: <T>(path: string) => request<T>("DELETE", path),
};
