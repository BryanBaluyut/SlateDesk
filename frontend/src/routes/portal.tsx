import { useQueryClient } from "@tanstack/react-query";
import {
  createFileRoute,
  Link,
  Outlet,
  redirect,
  useRouter,
} from "@tanstack/react-router";

import { logout, meQueryOptions } from "@/api/auth";
import { isApiError } from "@/api/client";
import { Button } from "@/components/ui/button";

/**
 * Customer portal layout — a clean, single-column self-service surface,
 * deliberately distinct from the agent workspace. Only customer-role users
 * belong here; agents/admins are bounced to the workspace.
 */
export const Route = createFileRoute("/portal")({
  beforeLoad: async ({ context, location }) => {
    let user;
    try {
      user = await context.queryClient.ensureQueryData(meQueryOptions);
    } catch (err) {
      if (isApiError(err) && err.status === 401) {
        throw redirect({ to: "/login", search: { redirect: location.href } });
      }
      throw err;
    }
    if (user.role !== "customer") {
      throw redirect({ to: "/" });
    }
    return { user };
  },
  component: PortalLayout,
});

function PortalLayout() {
  const { user } = Route.useRouteContext();
  const router = useRouter();
  const queryClient = useQueryClient();

  async function signOut() {
    await logout();
    queryClient.clear();
    router.navigate({ to: "/login" });
  }

  return (
    <div className="min-h-svh bg-muted/30">
      <header className="border-b bg-background">
        <div className="mx-auto flex max-w-3xl items-center justify-between px-4 py-3">
          <Link to="/portal" className="flex items-center gap-2">
            <span className="flex size-7 items-center justify-center rounded-md bg-primary text-sm font-semibold text-primary-foreground">
              S
            </span>
            <span className="text-sm font-semibold tracking-tight">
              Help Center
            </span>
          </Link>
          <div className="flex items-center gap-3">
            <span className="hidden text-sm text-muted-foreground sm:inline">
              {user.name}
            </span>
            <Button variant="ghost" size="sm" onClick={signOut}>
              Sign out
            </Button>
          </div>
        </div>
      </header>
      <main className="mx-auto max-w-3xl px-4 py-8">
        <Outlet />
      </main>
    </div>
  );
}
