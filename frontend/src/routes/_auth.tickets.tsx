import { createFileRoute } from "@tanstack/react-router";
import { Inbox } from "lucide-react";

import {
  PageContainer,
  PageHeader,
  PagePlaceholder,
} from "@/components/layout/page";

export const Route = createFileRoute("/_auth/tickets")({
  component: TicketsPage,
});

function TicketsPage() {
  return (
    <PageContainer>
      <PageHeader title="Tickets" description="Your team's shared inbox." />
      <PagePlaceholder
        icon={Inbox}
        title="No tickets yet"
        description="The ticket workspace ships in milestone 2 — three panes, keyboard-first, with the reply composer docked to the thread."
      />
    </PageContainer>
  );
}
