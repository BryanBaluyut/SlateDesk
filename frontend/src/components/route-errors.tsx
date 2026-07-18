import { Link, useRouter } from "@tanstack/react-router";
import { Compass, Loader2 } from "lucide-react";

import { ProblemAlert } from "@/components/problem-alert";
import { Button } from "@/components/ui/button";

/**
 * Router-level fallbacks: a calm panel instead of a raw overlay when a
 * loader/guard fails with a non-401 error, and a small 404 for unknown
 * in-app paths.
 */
export function RouteErrorFallback({ error }: { error: Error }) {
  const router = useRouter();
  return (
    <div className="grid min-h-svh place-items-center bg-background px-4">
      <div className="w-full max-w-sm space-y-4">
        <ProblemAlert error={error} />
        <div className="flex justify-center">
          <Button variant="outline" size="sm" onClick={() => router.invalidate()}>
            Try again
          </Button>
        </div>
      </div>
    </div>
  );
}

/**
 * Router-level pending fallback: entry navigations (cold load, a shared
 * /tickets/{id} link) block on loaders (/auth/me, ticket detail); show a
 * spinner instead of a blank viewport for the round-trip.
 */
export function RoutePending() {
  return (
    <div
      role="status"
      aria-label="Loading"
      className="grid min-h-svh place-items-center bg-background"
    >
      <Loader2
        className="size-5 animate-spin text-muted-foreground"
        aria-hidden="true"
      />
    </div>
  );
}

export function RouteNotFound() {
  return (
    <div className="grid min-h-svh place-items-center bg-background px-4">
      <div className="flex flex-col items-center gap-3 text-center">
        <div className="flex size-10 items-center justify-center rounded-full border bg-muted/50">
          <Compass className="size-5 text-muted-foreground" aria-hidden="true" />
        </div>
        <div>
          <h1 className="text-sm font-medium">Page not found</h1>
          <p className="mt-1 text-sm text-muted-foreground">
            Nothing lives at this address.
          </p>
        </div>
        <Button asChild variant="outline" size="sm">
          <Link to="/">Back home</Link>
        </Button>
      </div>
    </div>
  );
}
