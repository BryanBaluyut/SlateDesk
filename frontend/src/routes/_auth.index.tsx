import { createFileRoute, redirect } from "@tanstack/react-router";

/** Tickets is home once M2 lands; the placeholder already plays that role. */
export const Route = createFileRoute("/_auth/")({
  beforeLoad: () => {
    throw redirect({ to: "/tickets" });
  },
});
