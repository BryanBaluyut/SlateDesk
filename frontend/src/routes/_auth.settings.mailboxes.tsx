import { useQuery } from "@tanstack/react-query";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { Check, Ellipsis, Mail, Plus, X } from "lucide-react";
import { useEffect, useState } from "react";

import { useDeleteMailbox, mailboxesQueryOptions } from "@/api/mailboxes";
import type { Mailbox } from "@/api/types";
import { MAILBOX_AUTH_KIND_LABELS } from "@/api/types";
import {
  PagePlaceholder,
} from "@/components/layout/page";
import { GOOGLE_CONNECTED_KEY, MailboxDialog } from "@/components/mailboxes/mailbox-dialog";
import { MailboxHealthPill } from "@/components/mailboxes/health-pill";
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
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
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

interface MailboxesSearch {
  /** Set by the Google OAuth callback redirect (?connected=1). */
  connected?: number;
}

export const Route = createFileRoute("/_auth/settings/mailboxes")({
  validateSearch: (search: Record<string, unknown>): MailboxesSearch => ({
    connected: search.connected === 1 || search.connected === "1" ? 1 : undefined,
  }),
  component: MailboxesPage,
});

function MailboxesPage() {
  const { connected } = Route.useSearch();
  const navigate = useNavigate({ from: Route.fullPath });

  const mailboxesQuery = useQuery(mailboxesQueryOptions);
  const deleteMailbox = useDeleteMailbox();

  const [createOpen, setCreateOpen] = useState(false);
  const [editing, setEditing] = useState<Mailbox | null>(null);
  const [deleting, setDeleting] = useState<Mailbox | null>(null);
  const [connectedBanner, setConnectedBanner] = useState(false);

  // The Google OAuth callback lands (in its own window) on
  // /settings/mailboxes?connected=1. Show the banner, tell any opener
  // window's dialog via localStorage (storage events cross windows), and
  // strip the param so a reload doesn't re-announce.
  useEffect(() => {
    if (connected === 1) {
      setConnectedBanner(true);
      try {
        localStorage.setItem(GOOGLE_CONNECTED_KEY, String(Date.now()));
      } catch {
        // Storage unavailable: the banner alone still tells this window.
      }
      void navigate({ search: {}, replace: true });
    }
  }, [connected, navigate]);

  const mailboxes = mailboxesQuery.data ?? [];

  return (
    <div>
      <div className="flex items-start justify-between gap-4 pb-4">
        <div>
          <h2 className="text-sm font-medium">Mailboxes</h2>
          <p className="mt-0.5 text-sm text-muted-foreground">
            Email accounts SlateDesk polls and sends through.
          </p>
        </div>
        <Button size="sm" onClick={() => setCreateOpen(true)}>
          <Plus aria-hidden="true" />
          Add mailbox
        </Button>
      </div>

      {connectedBanner && (
        <div className="mb-4 flex items-center gap-2 rounded-md border border-emerald-500/40 bg-emerald-500/10 px-3 py-2 text-sm">
          <Check
            className="size-4 shrink-0 text-emerald-600 dark:text-emerald-400"
            aria-hidden="true"
          />
          <span className="flex-1">
            Google account connected. The mailbox can now poll and send.
          </span>
          <button
            type="button"
            onClick={() => setConnectedBanner(false)}
            aria-label="Dismiss"
            className="rounded p-0.5 text-muted-foreground hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            <X className="size-3.5" aria-hidden="true" />
          </button>
        </div>
      )}

      {mailboxesQuery.isError && <ProblemAlert error={mailboxesQuery.error} />}

      {mailboxesQuery.isPending && (
        <div className="space-y-2">
          {Array.from({ length: 2 }, (_, i) => (
            <Skeleton key={i} className="h-11 w-full" />
          ))}
        </div>
      )}

      {mailboxesQuery.isSuccess && mailboxes.length === 0 && (
        <PagePlaceholder
          icon={Mail}
          title="No mailboxes yet"
          description="Connect a support address — Microsoft 365, Google, or plain IMAP/SMTP — and inbound mail becomes tickets."
        />
      )}

      {mailboxesQuery.isSuccess && mailboxes.length > 0 && (
        <div className="overflow-x-auto rounded-lg border">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Mailbox</TableHead>
                <TableHead>Auth</TableHead>
                <TableHead>Health</TableHead>
                <TableHead>Last poll</TableHead>
                <TableHead className="w-12" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {mailboxes.map((mailbox) => (
                <TableRow
                  key={mailbox.id}
                  className={cn(!mailbox.active && "opacity-60")}
                >
                  <TableCell>
                    <div className="min-w-0">
                      <div className="truncate font-medium">{mailbox.name}</div>
                      <div className="truncate text-xs text-muted-foreground">
                        {mailbox.email_address}
                      </div>
                    </div>
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {MAILBOX_AUTH_KIND_LABELS[mailbox.auth_kind]}
                  </TableCell>
                  <TableCell>
                    <MailboxHealthPill mailbox={mailbox} />
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {mailbox.last_poll_at
                      ? formatRelativeTime(mailbox.last_poll_at)
                      : "—"}
                  </TableCell>
                  <TableCell>
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button
                          variant="ghost"
                          size="icon-xs"
                          aria-label={`Actions for ${mailbox.name}`}
                        >
                          <Ellipsis aria-hidden="true" />
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem onSelect={() => setEditing(mailbox)}>
                          Edit
                        </DropdownMenuItem>
                        <DropdownMenuSeparator />
                        <DropdownMenuItem
                          variant="destructive"
                          onSelect={() => setDeleting(mailbox)}
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

      <MailboxDialog open={createOpen} onOpenChange={setCreateOpen} />
      <MailboxDialog
        open={editing !== null}
        onOpenChange={(open) => {
          if (!open) setEditing(null);
        }}
        mailbox={editing ?? undefined}
      />

      <AlertDialog
        open={deleting !== null}
        onOpenChange={(open) => {
          if (!open) setDeleting(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete {deleting?.name}?</AlertDialogTitle>
            <AlertDialogDescription>
              Polling and sending through {deleting?.email_address} stops
              immediately and its credentials are erased. Existing tickets
              and their email threading are kept.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (deleting) {
                  deleteMailbox.mutate(deleting.id);
                }
              }}
            >
              Delete mailbox
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
