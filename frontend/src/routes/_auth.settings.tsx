import { createFileRoute } from "@tanstack/react-router";
import { Settings } from "lucide-react";

import {
  PageContainer,
  PageHeader,
  PagePlaceholder,
} from "@/components/layout/page";

export const Route = createFileRoute("/_auth/settings")({
  component: SettingsPage,
});

function SettingsPage() {
  return (
    <PageContainer>
      <PageHeader
        title="Settings"
        description="Instance configuration and branding."
      />
      <PagePlaceholder
        icon={Settings}
        title="Nothing to configure yet"
        description="Instance settings — name, external URL, mailboxes — arrive alongside the email milestone."
      />
    </PageContainer>
  );
}
