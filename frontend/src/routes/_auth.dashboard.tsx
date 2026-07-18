import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import type { LucideIcon } from "lucide-react";
import { CheckCircle2, Clock3, Inbox, UserRoundX } from "lucide-react";

import { dashboardCountersQueryOptions } from "@/api/dashboard";
import type { DashboardCounters } from "@/api/types";
import { PageContainer, PageHeader } from "@/components/layout/page";
import { ProblemAlert } from "@/components/problem-alert";
import { Skeleton } from "@/components/ui/skeleton";
import type { TicketsSearch } from "./_auth.tickets";

export const Route = createFileRoute("/_auth/dashboard")({
  component: DashboardPage,
});

interface Tile {
  key: keyof DashboardCounters;
  label: string;
  icon: LucideIcon;
  /** Pre-filtered queue view this tile links into. */
  search: TicketsSearch;
}

const TILES: Tile[] = [
  {
    key: "open",
    label: "Open",
    icon: Inbox,
    search: { view: "open", status: ["open"] },
  },
  {
    key: "unassigned",
    label: "Unassigned",
    icon: UserRoundX,
    search: { view: "unassigned" },
  },
  {
    key: "waiting_on_customer",
    label: "Waiting on customer",
    icon: Clock3,
    search: { view: "open", status: ["waiting_on_customer"] },
  },
  {
    // closed_today scopes the Closed view to the counter's own predicate,
    // so the list this tile opens matches the number it shows.
    key: "closed_today",
    label: "Closed today",
    icon: CheckCircle2,
    search: { view: "closed", closed_today: true },
  },
];

/** Exactly four counters, no charts — calm by design. */
function DashboardPage() {
  const countersQuery = useQuery(dashboardCountersQueryOptions);

  return (
    <PageContainer>
      <PageHeader
        title="Dashboard"
        description="The workspace at a glance. Each tile opens the matching queue view."
      />

      {countersQuery.isError && <ProblemAlert error={countersQuery.error} />}

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {TILES.map((tile) => (
          <Link
            key={tile.key}
            to="/tickets"
            search={tile.search}
            className="group rounded-lg border p-4 transition-colors hover:bg-accent/50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            <div className="flex items-center gap-2 text-sm text-muted-foreground">
              <tile.icon className="size-4" aria-hidden="true" />
              {tile.label}
            </div>
            {countersQuery.isSuccess ? (
              <div className="mt-2 text-3xl font-semibold tabular-nums tracking-tight">
                {countersQuery.data[tile.key]}
              </div>
            ) : (
              <Skeleton className="mt-3 h-8 w-16" />
            )}
          </Link>
        ))}
      </div>
    </PageContainer>
  );
}
