import { queryOptions } from "@tanstack/react-query";

import { api } from "./client";
import type { DashboardCounters } from "./types";

export const dashboardCountersQueryOptions = queryOptions({
  queryKey: ["dashboard", "counters"],
  queryFn: () => api.get<DashboardCounters>("/dashboard/counters"),
});
