import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import { Check } from "lucide-react";
import { useEffect, useState } from "react";

import { externalUrlQueryOptions, useSetExternalUrl } from "@/api/mailboxes";
import { ProblemAlert } from "@/components/problem-alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";

export const Route = createFileRoute("/_auth/settings/")({
  component: GeneralSettingsPage,
});

/**
 * General instance settings. Just the external URL for now — the Google
 * OAuth connect flow needs it to build the redirect_uri, and outbound
 * email links will use it later.
 */
function GeneralSettingsPage() {
  const settingQuery = useQuery(externalUrlQueryOptions);
  const setExternalUrl = useSetExternalUrl();

  const [value, setValue] = useState("");
  const [saved, setSaved] = useState(false);

  // Seed the input once the setting arrives (and re-seed on refetch only
  // if the user has not diverged — simplest: seed when data changes and
  // the field still holds the previous server value).
  const serverValue = settingQuery.data?.external_url;
  useEffect(() => {
    if (serverValue !== undefined) {
      setValue(serverValue);
    }
  }, [serverValue]);

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    setSaved(false);
    try {
      await setExternalUrl.mutateAsync(value.trim());
      setSaved(true);
      setTimeout(() => setSaved(false), 2000);
    } catch {
      // Rendered via setExternalUrl.error below.
    }
  }

  return (
    <div className="max-w-lg">
      <h2 className="text-sm font-medium">External URL</h2>
      <p className="mt-1 text-sm text-muted-foreground">
        The public base URL this instance is reached at — no path, no
        trailing slash. Required before connecting a Google mailbox (it
        forms the OAuth redirect URI).
      </p>

      {settingQuery.isPending ? (
        <Skeleton className="mt-4 h-9 w-full" />
      ) : settingQuery.isError ? (
        <div className="mt-4">
          <ProblemAlert error={settingQuery.error} />
        </div>
      ) : (
        <form onSubmit={onSubmit} className="mt-4 space-y-3" noValidate>
          {setExternalUrl.isError && (
            <ProblemAlert error={setExternalUrl.error} />
          )}
          <div className="space-y-2">
            <Label htmlFor="external-url">Base URL</Label>
            <Input
              id="external-url"
              type="url"
              placeholder="https://desk.example.com"
              autoComplete="off"
              value={value}
              onChange={(e) => setValue(e.target.value)}
            />
          </div>
          <div className="flex items-center gap-2">
            <Button
              type="submit"
              size="sm"
              disabled={setExternalUrl.isPending || value.trim() === ""}
            >
              {setExternalUrl.isPending ? "Saving…" : "Save"}
            </Button>
            {saved && (
              <span className="flex items-center gap-1 text-sm text-emerald-600 dark:text-emerald-400">
                <Check className="size-3.5" aria-hidden="true" />
                Saved
              </span>
            )}
          </div>
        </form>
      )}
    </div>
  );
}
