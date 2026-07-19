import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import { Check, Copy, Ellipsis, KeyRound, Plus, TriangleAlert } from "lucide-react";
import { useState } from "react";

import {
  apiKeysQueryOptions,
  useCreateApiKey,
  useRevokeApiKey,
} from "@/api/apikeys";
import type { ApiKey, ApiKeyCreated, ApiKeyScope } from "@/api/types";
import { API_KEY_SCOPES, API_KEY_SCOPE_LABELS } from "@/api/types";
import { PagePlaceholder } from "@/components/layout/page";
import { ProblemAlert } from "@/components/problem-alert";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatRelativeTime } from "@/lib/time";
import { cn } from "@/lib/utils";

export const Route = createFileRoute("/_auth/settings/api-keys")({
  component: ApiKeysPage,
});

function ApiKeysPage() {
  const keysQuery = useQuery(apiKeysQueryOptions);
  const [createOpen, setCreateOpen] = useState(false);
  const [revoking, setRevoking] = useState<ApiKey | null>(null);
  const revoke = useRevokeApiKey();

  const keys = keysQuery.data ?? [];

  return (
    <div>
      <div className="flex items-start justify-between gap-4 pb-4">
        <div>
          <h2 className="text-sm font-medium">API keys</h2>
          <p className="mt-0.5 text-sm text-muted-foreground">
            Scoped machine credentials for the REST API. Present as{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-xs">
              Authorization: Bearer sd_live_…
            </code>
          </p>
        </div>
        <Button size="sm" onClick={() => setCreateOpen(true)}>
          <Plus aria-hidden="true" />
          Create key
        </Button>
      </div>

      {keysQuery.isError && <ProblemAlert error={keysQuery.error} />}

      {keysQuery.isPending && (
        <div className="space-y-2">
          {Array.from({ length: 2 }, (_, i) => (
            <Skeleton key={i} className="h-11 w-full" />
          ))}
        </div>
      )}

      {keysQuery.isSuccess && keys.length === 0 && (
        <PagePlaceholder
          icon={KeyRound}
          title="No API keys yet"
          description="Create a scoped key to let scripts and integrations call the SlateDesk API."
        />
      )}

      {keysQuery.isSuccess && keys.length > 0 && (
        <div className="overflow-x-auto rounded-lg border">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Name</TableHead>
                <TableHead>Prefix</TableHead>
                <TableHead>Scopes</TableHead>
                <TableHead>Last used</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="w-12" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {keys.map((key) => {
                const revoked = key.revoked_at !== null;
                return (
                  <TableRow key={key.id} className={cn(revoked && "opacity-60")}>
                    <TableCell className="font-medium">{key.name}</TableCell>
                    <TableCell>
                      <code className="text-xs text-muted-foreground">
                        {key.key_prefix}…
                      </code>
                    </TableCell>
                    <TableCell>
                      <div className="flex flex-wrap gap-1">
                        {key.scopes.map((scope) => (
                          <Badge key={scope} variant="secondary">
                            {API_KEY_SCOPE_LABELS[scope]}
                          </Badge>
                        ))}
                      </div>
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {key.last_used_at
                        ? formatRelativeTime(key.last_used_at)
                        : "Never"}
                    </TableCell>
                    <TableCell>
                      {revoked ? (
                        <Badge variant="outline">Revoked</Badge>
                      ) : (
                        <Badge variant="secondary">Active</Badge>
                      )}
                    </TableCell>
                    <TableCell>
                      {!revoked && (
                        <DropdownMenu>
                          <DropdownMenuTrigger asChild>
                            <Button
                              variant="ghost"
                              size="icon-xs"
                              aria-label={`Actions for ${key.name}`}
                            >
                              <Ellipsis aria-hidden="true" />
                            </Button>
                          </DropdownMenuTrigger>
                          <DropdownMenuContent align="end">
                            <DropdownMenuItem
                              variant="destructive"
                              onSelect={() => setRevoking(key)}
                            >
                              Revoke
                            </DropdownMenuItem>
                          </DropdownMenuContent>
                        </DropdownMenu>
                      )}
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </div>
      )}

      <CreateApiKeyDialog open={createOpen} onOpenChange={setCreateOpen} />

      <AlertDialog
        open={revoking !== null}
        onOpenChange={(open) => !open && setRevoking(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Revoke {revoking?.name}?</AlertDialogTitle>
            <AlertDialogDescription>
              The key stops authenticating immediately. Anything using it will
              start getting 401s. This cannot be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (revoking) revoke.mutate(revoking.id);
              }}
            >
              Revoke key
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function CreateApiKeyDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const create = useCreateApiKey();
  const [name, setName] = useState("");
  const [scopes, setScopes] = useState<ApiKeyScope[]>(["read"]);
  const [error, setError] = useState<unknown>(null);
  const [created, setCreated] = useState<ApiKeyCreated | null>(null);

  function reset() {
    setName("");
    setScopes(["read"]);
    setError(null);
    setCreated(null);
  }

  function toggleScope(scope: ApiKeyScope) {
    setScopes((prev) =>
      prev.includes(scope) ? prev.filter((s) => s !== scope) : [...prev, scope],
    );
  }

  async function submit() {
    setError(null);
    try {
      const result = await create.mutateAsync({ name: name.trim(), scopes });
      setCreated(result);
    } catch (err) {
      setError(err);
    }
  }

  const canSubmit = name.trim().length > 0 && scopes.length > 0;

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        onOpenChange(next);
        if (!next) reset();
      }}
    >
      <DialogContent className="sm:max-w-md">
        {created ? (
          <>
            <DialogHeader>
              <DialogTitle>Copy your API key</DialogTitle>
              <DialogDescription>
                This is the only time the full key is shown. Store it securely —
                you cannot retrieve it again.
              </DialogDescription>
            </DialogHeader>
            <SecretReveal value={created.key} label="API key" />
            <DialogFooter>
              <Button onClick={() => onOpenChange(false)}>Done</Button>
            </DialogFooter>
          </>
        ) : (
          <>
            <DialogHeader>
              <DialogTitle>Create API key</DialogTitle>
              <DialogDescription>
                The plaintext key is shown once, right after creation.
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-4">
              {error != null && <ProblemAlert error={error} />}
              <div className="space-y-2">
                <Label htmlFor="key-name">Name</Label>
                <Input
                  id="key-name"
                  autoComplete="off"
                  placeholder="CI deploy, Zapier, …"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                />
              </div>
              <div className="space-y-2">
                <Label>Scopes</Label>
                <div className="space-y-2">
                  {API_KEY_SCOPES.map((scope) => (
                    <label
                      key={scope}
                      className="flex items-start gap-2.5 text-sm"
                    >
                      <Checkbox
                        checked={scopes.includes(scope)}
                        onCheckedChange={() => toggleScope(scope)}
                      />
                      <span>
                        <span className="font-medium">
                          {API_KEY_SCOPE_LABELS[scope]}
                        </span>
                        <span className="block text-xs text-muted-foreground">
                          {scope === "read"
                            ? "GET requests only."
                            : "Create, update, and delete."}
                        </span>
                      </span>
                    </label>
                  ))}
                </div>
              </div>
            </div>
            <DialogFooter>
              <Button variant="outline" onClick={() => onOpenChange(false)}>
                Cancel
              </Button>
              <Button
                onClick={() => void submit()}
                disabled={!canSubmit || create.isPending}
              >
                {create.isPending ? "Creating…" : "Create key"}
              </Button>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}

/** One-time secret reveal with a copy button and a plain warning. */
export function SecretReveal({
  value,
  label,
}: {
  value: string;
  label: string;
}) {
  const [copied, setCopied] = useState(false);

  async function copy() {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      // Clipboard blocked (insecure context): the value is selectable inline.
    }
  }

  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2 rounded-md border bg-muted/50 p-2">
        <code className="flex-1 select-all break-all font-mono text-xs">
          {value}
        </code>
        <Button
          type="button"
          variant="outline"
          size="icon-sm"
          aria-label={`Copy ${label}`}
          onClick={() => void copy()}
        >
          {copied ? (
            <Check className="text-emerald-600 dark:text-emerald-400" aria-hidden="true" />
          ) : (
            <Copy aria-hidden="true" />
          )}
        </Button>
      </div>
      <p className="flex items-start gap-1.5 text-xs text-amber-700 dark:text-amber-300">
        <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
        <span>Shown only once. It cannot be retrieved later.</span>
      </p>
    </div>
  );
}
