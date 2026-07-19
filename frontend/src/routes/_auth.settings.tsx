import { createFileRoute, Link, Outlet, redirect } from "@tanstack/react-router";

import { PageContainer, PageHeader } from "@/components/layout/page";
import { cn } from "@/lib/utils";

/**
 * Admin-only settings group. The API rejects non-admins on every
 * /mailboxes and /settings endpoint anyway; this gate just keeps them
 * from landing on a page of 403s. The sidebar hides the entry too.
 */
export const Route = createFileRoute("/_auth/settings")({
  beforeLoad: ({ context }) => {
    if (context.user.role !== "admin") {
      throw redirect({ to: "/tickets" });
    }
  },
  component: SettingsLayout,
});

const SECTIONS = [
  { to: "/settings", label: "General", exact: true },
  { to: "/settings/mailboxes", label: "Mailboxes", exact: false },
  { to: "/settings/api-keys", label: "API keys", exact: false },
  { to: "/settings/webhooks", label: "Webhooks", exact: false },
] as const;

function SettingsLayout() {
  return (
    <PageContainer>
      <PageHeader
        title="Settings"
        description="Instance configuration. Admin only."
      />
      <nav
        aria-label="Settings sections"
        className="mb-6 flex gap-1 border-b pb-px"
      >
        {SECTIONS.map((section) => (
          <Link
            key={section.to}
            to={section.to}
            activeOptions={{ exact: section.exact }}
            className={cn(
              "-mb-px rounded-t-md border-b-2 border-transparent px-3 py-1.5 text-sm text-muted-foreground",
              "hover:text-foreground",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
            )}
            activeProps={{
              className: "border-primary font-medium text-foreground",
              "aria-current": "page",
            }}
          >
            {section.label}
          </Link>
        ))}
      </nav>
      <Outlet />
    </PageContainer>
  );
}
