import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import {
  CircleCheck,
  CircleX,
  Ellipsis,
  Plus,
  Send,
  Webhook as WebhookIcon,
} from "lucide-react";
import { useState } from "react";

import { SecretReveal } from "./_auth.settings.api-keys";
import {
  useCreateWebhook,
  useDeleteWebhook,
  useUpdateWebhook,
  webhookDeliveriesQueryOptions,
  webhooksQueryOptions,
  testWebhook,
} from "@/api/webhooks";
import type {
  Webhook,
  WebhookCreated,
  WebhookEvent,
  WebhookTestResult,
} from "@/api/types";
import {
  WEBHOOK_DELIVERY_STATUS_LABELS,
  WEBHOOK_EVENTS,
  WEBHOOK_EVENT_LABELS,
} from "@/api/types";
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
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
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

export const Route = createFileRoute("/_auth/settings/webhooks")({
  component: WebhooksPage,
});

function WebhooksPage() {
  const webhooksQuery = useQuery(webhooksQueryOptions);
  const updateWebhook = useUpdateWebhook();
  const deleteWebhook = useDeleteWebhook();

  const [createOpen, setCreateOpen] = useState(false);
  const [editing, setEditing] = useState<Webhook | null>(null);
  const [deleting, setDeleting] = useState<Webhook | null>(null);
  const [deliveriesFor, setDeliveriesFor] = useState<Webhook | null>(null);
  const [testing, setTesting] = useState<string | null>(null);
  const [testResult, setTestResult] = useState<
    { name: string; result: WebhookTestResult } | null
  >(null);

  const webhooks = webhooksQuery.data ?? [];

  async function runTest(webhook: Webhook) {
    setTesting(webhook.id);
    setTestResult(null);
    try {
      const result = await testWebhook(webhook.id);
      setTestResult({ name: webhook.name, result });
    } finally {
      setTesting(null);
    }
  }

  return (
    <div>
      <div className="flex items-start justify-between gap-4 pb-4">
        <div>
          <h2 className="text-sm font-medium">Webhooks</h2>
          <p className="mt-0.5 text-sm text-muted-foreground">
            Outbound HTTP notifications on ticket and article events, signed
            with{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-xs">
              X-SlateDesk-Signature
            </code>
            .
          </p>
        </div>
        <Button size="sm" onClick={() => setCreateOpen(true)}>
          <Plus aria-hidden="true" />
          Add webhook
        </Button>
      </div>

      {testResult && (
        <div
          className={cn(
            "mb-4 flex items-start gap-2 rounded-md border px-3 py-2 text-sm",
            testResult.result.ok
              ? "border-emerald-500/40 bg-emerald-500/10"
              : "border-destructive/40 bg-destructive/10",
          )}
        >
          {testResult.result.ok ? (
            <CircleCheck className="mt-0.5 size-4 shrink-0 text-emerald-600 dark:text-emerald-400" aria-hidden="true" />
          ) : (
            <CircleX className="mt-0.5 size-4 shrink-0 text-destructive" aria-hidden="true" />
          )}
          <span className="flex-1">
            <span className="font-medium">Test to {testResult.name}: </span>
            {testResult.result.detail}
            {testResult.result.response_code != null &&
              ` (HTTP ${testResult.result.response_code})`}
            {` · ${testResult.result.latency_ms}ms`}
          </span>
          <button
            type="button"
            onClick={() => setTestResult(null)}
            aria-label="Dismiss"
            className="rounded p-0.5 text-muted-foreground hover:text-foreground"
          >
            <CircleX className="size-3.5" aria-hidden="true" />
          </button>
        </div>
      )}

      {webhooksQuery.isError && <ProblemAlert error={webhooksQuery.error} />}

      {webhooksQuery.isPending && (
        <div className="space-y-2">
          {Array.from({ length: 2 }, (_, i) => (
            <Skeleton key={i} className="h-11 w-full" />
          ))}
        </div>
      )}

      {webhooksQuery.isSuccess && webhooks.length === 0 && (
        <PagePlaceholder
          icon={WebhookIcon}
          title="No webhooks yet"
          description="Register an endpoint to receive signed JSON on ticket.created, ticket.updated, and article.created."
        />
      )}

      {webhooksQuery.isSuccess && webhooks.length > 0 && (
        <div className="overflow-x-auto rounded-lg border">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Endpoint</TableHead>
                <TableHead>Events</TableHead>
                <TableHead>Active</TableHead>
                <TableHead className="w-12" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {webhooks.map((webhook) => (
                <TableRow
                  key={webhook.id}
                  className={cn(!webhook.active && "opacity-60")}
                >
                  <TableCell>
                    <div className="min-w-0">
                      <div className="truncate font-medium">{webhook.name}</div>
                      <div className="truncate text-xs text-muted-foreground">
                        {webhook.url}
                      </div>
                    </div>
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-wrap gap-1">
                      {webhook.events.map((event) => (
                        <Badge key={event} variant="secondary">
                          {WEBHOOK_EVENT_LABELS[event]}
                        </Badge>
                      ))}
                    </div>
                  </TableCell>
                  <TableCell>
                    <Switch
                      checked={webhook.active}
                      aria-label={`${webhook.active ? "Disable" : "Enable"} ${webhook.name}`}
                      onCheckedChange={(active) =>
                        updateWebhook.mutate({
                          id: webhook.id,
                          body: { active },
                        })
                      }
                    />
                  </TableCell>
                  <TableCell>
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button
                          variant="ghost"
                          size="icon-xs"
                          aria-label={`Actions for ${webhook.name}`}
                        >
                          <Ellipsis aria-hidden="true" />
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem
                          onSelect={() => void runTest(webhook)}
                          disabled={testing === webhook.id}
                        >
                          <Send aria-hidden="true" />
                          {testing === webhook.id ? "Sending…" : "Send test"}
                        </DropdownMenuItem>
                        <DropdownMenuItem
                          onSelect={() => setDeliveriesFor(webhook)}
                        >
                          Recent deliveries
                        </DropdownMenuItem>
                        <DropdownMenuItem onSelect={() => setEditing(webhook)}>
                          Edit
                        </DropdownMenuItem>
                        <DropdownMenuSeparator />
                        <DropdownMenuItem
                          variant="destructive"
                          onSelect={() => setDeleting(webhook)}
                        >
                          Delete
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      <WebhookDialog open={createOpen} onOpenChange={setCreateOpen} />
      <WebhookDialog
        open={editing !== null}
        onOpenChange={(open) => !open && setEditing(null)}
        webhook={editing ?? undefined}
      />

      <DeliveriesDialog
        webhook={deliveriesFor}
        onOpenChange={(open) => !open && setDeliveriesFor(null)}
      />

      <AlertDialog
        open={deleting !== null}
        onOpenChange={(open) => !open && setDeleting(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete {deleting?.name}?</AlertDialogTitle>
            <AlertDialogDescription>
              The endpoint stops receiving events and its delivery log is
              removed. This cannot be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (deleting) deleteWebhook.mutate(deleting.id);
              }}
            >
              Delete webhook
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function WebhookDialog({
  open,
  onOpenChange,
  webhook,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  webhook?: Webhook;
}) {
  const isEdit = webhook !== undefined;
  const create = useCreateWebhook();
  const update = useUpdateWebhook();

  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [events, setEvents] = useState<WebhookEvent[]>([]);
  const [secret, setSecret] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [createdSecret, setCreatedSecret] = useState<string | null>(null);
  const [initialised, setInitialised] = useState(false);

  // Seed / reset when the dialog opens.
  if (open && !initialised) {
    setName(webhook?.name ?? "");
    setUrl(webhook?.url ?? "");
    setEvents(webhook ? [...webhook.events] : ["ticket.created"]);
    setSecret("");
    setError(null);
    setCreatedSecret(null);
    setInitialised(true);
  }

  function close(next: boolean) {
    onOpenChange(next);
    if (!next) setInitialised(false);
  }

  function toggleEvent(event: WebhookEvent) {
    setEvents((prev) =>
      prev.includes(event) ? prev.filter((e) => e !== event) : [...prev, event],
    );
  }

  async function submit() {
    setError(null);
    try {
      if (isEdit) {
        await update.mutateAsync({
          id: webhook.id,
          body: {
            name: name.trim(),
            url: url.trim(),
            events,
            ...(secret.trim() ? { secret: secret.trim() } : {}),
          },
        });
        close(false);
      } else {
        const result: WebhookCreated = await create.mutateAsync({
          name: name.trim(),
          url: url.trim(),
          events,
          active: true,
          ...(secret.trim() ? { secret: secret.trim() } : {}),
        });
        setCreatedSecret(result.signing_secret);
      }
    } catch (err) {
      setError(err);
    }
  }

  const pending = create.isPending || update.isPending;
  const canSubmit = name.trim() && url.trim() && events.length > 0;

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="sm:max-w-md">
        {createdSecret ? (
          <>
            <DialogHeader>
              <DialogTitle>Copy the signing secret</DialogTitle>
              <DialogDescription>
                Configure this on your receiver to verify the
                {" "}
                <code className="rounded bg-muted px-1 py-0.5 text-xs">
                  X-SlateDesk-Signature
                </code>{" "}
                header. It is shown only once.
              </DialogDescription>
            </DialogHeader>
            <SecretReveal value={createdSecret} label="signing secret" />
            <DialogFooter>
              <Button onClick={() => close(false)}>Done</Button>
            </DialogFooter>
          </>
        ) : (
          <>
            <DialogHeader>
              <DialogTitle>{isEdit ? "Edit webhook" : "New webhook"}</DialogTitle>
              <DialogDescription>
                {isEdit
                  ? "Leave the secret blank to keep the current one."
                  : "A signing secret is generated if you leave it blank, and shown once."}
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-4">
              {error != null && <ProblemAlert error={error} />}
              <div className="space-y-2">
                <Label htmlFor="wh-name">Name</Label>
                <Input
                  id="wh-name"
                  autoComplete="off"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="wh-url">Payload URL</Label>
                <Input
                  id="wh-url"
                  type="url"
                  placeholder="https://example.com/hooks/slatedesk"
                  autoComplete="off"
                  value={url}
                  onChange={(e) => setUrl(e.target.value)}
                />
              </div>
              <div className="space-y-2">
                <Label>Events</Label>
                <div className="space-y-2">
                  {WEBHOOK_EVENTS.map((event) => (
                    <label
                      key={event}
                      className="flex items-center gap-2.5 text-sm"
                    >
                      <Checkbox
                        checked={events.includes(event)}
                        onCheckedChange={() => toggleEvent(event)}
                      />
                      <span>{WEBHOOK_EVENT_LABELS[event]}</span>
                      <code className="ml-auto text-xs text-muted-foreground">
                        {event}
                      </code>
                    </label>
                  ))}
                </div>
              </div>
              <div className="space-y-2">
                <Label htmlFor="wh-secret">
                  Signing secret{" "}
                  <span className="font-normal text-muted-foreground">
                    (optional)
                  </span>
                </Label>
                <Input
                  id="wh-secret"
                  autoComplete="off"
                  placeholder={isEdit ? "Leave blank to keep current" : "Auto-generated if blank"}
                  value={secret}
                  onChange={(e) => setSecret(e.target.value)}
                />
              </div>
            </div>
            <DialogFooter>
              <Button variant="outline" onClick={() => close(false)}>
                Cancel
              </Button>
              <Button onClick={() => void submit()} disabled={!canSubmit || pending}>
                {pending ? "Saving…" : isEdit ? "Save changes" : "Create webhook"}
              </Button>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}

function DeliveriesDialog({
  webhook,
  onOpenChange,
}: {
  webhook: Webhook | null;
  onOpenChange: (open: boolean) => void;
}) {
  const query = useQuery({
    ...webhookDeliveriesQueryOptions(webhook?.id ?? ""),
    enabled: webhook !== null,
  });
  const deliveries = query.data ?? [];

  return (
    <Dialog open={webhook !== null} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Recent deliveries</DialogTitle>
          <DialogDescription>
            The durable outbox log for {webhook?.name}. Failed attempts retry
            with backoff.
          </DialogDescription>
        </DialogHeader>
        {query.isPending && (
          <div className="space-y-2">
            {Array.from({ length: 3 }, (_, i) => (
              <Skeleton key={i} className="h-9 w-full" />
            ))}
          </div>
        )}
        {query.isSuccess && deliveries.length === 0 && (
          <p className="py-6 text-center text-sm text-muted-foreground">
            No deliveries yet.
          </p>
        )}
        {deliveries.length > 0 && (
          <div className="max-h-96 overflow-y-auto rounded-lg border">
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>Event</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Code</TableHead>
                  <TableHead>Tries</TableHead>
                  <TableHead>When</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {deliveries.map((delivery) => (
                  <TableRow key={delivery.id}>
                    <TableCell>
                      <code className="text-xs">{delivery.event_type}</code>
                    </TableCell>
                    <TableCell>
                      <Badge
                        variant={
                          delivery.status === "success"
                            ? "secondary"
                            : delivery.status === "failed"
                              ? "destructive"
                              : "outline"
                        }
                      >
                        {WEBHOOK_DELIVERY_STATUS_LABELS[delivery.status]}
                      </Badge>
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {delivery.response_code ?? "—"}
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {delivery.attempts}
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {formatRelativeTime(
                        delivery.delivered_at ?? delivery.created_at,
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
