import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import { Ellipsis, MessageSquareText, Plus } from "lucide-react";
import { useState } from "react";

import {
  cannedRepliesQueryOptions,
  useCreateCannedReply,
  useDeleteCannedReply,
  useUpdateCannedReply,
} from "@/api/cannedReplies";
import type { CannedReply } from "@/api/types";
import {
  PageContainer,
  PageHeader,
  PagePlaceholder,
} from "@/components/layout/page";
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
import { Textarea } from "@/components/ui/textarea";
import { CANNED_PLACEHOLDERS } from "@/lib/canned";

export const Route = createFileRoute("/_auth/canned-replies")({
  component: CannedRepliesPage,
});

function CannedRepliesPage() {
  const repliesQuery = useQuery(cannedRepliesQueryOptions);
  const deleteReply = useDeleteCannedReply();

  const [createOpen, setCreateOpen] = useState(false);
  const [editing, setEditing] = useState<CannedReply | null>(null);
  const [deleting, setDeleting] = useState<CannedReply | null>(null);

  const replies = repliesQuery.data ?? [];

  return (
    <PageContainer>
      <div className="flex items-start justify-between gap-4">
        <PageHeader
          title="Canned replies"
          description="Reusable snippets for the reply composer. Placeholders are filled in from the ticket when you insert one."
        />
        <Button size="sm" onClick={() => setCreateOpen(true)}>
          <Plus aria-hidden="true" />
          New reply
        </Button>
      </div>

      {repliesQuery.isError && <ProblemAlert error={repliesQuery.error} />}

      {repliesQuery.isPending && (
        <div className="space-y-2">
          {Array.from({ length: 3 }, (_, i) => (
            <Skeleton key={i} className="h-16 w-full" />
          ))}
        </div>
      )}

      {repliesQuery.isSuccess && replies.length === 0 && (
        <PagePlaceholder
          icon={MessageSquareText}
          title="No canned replies yet"
          description="Save a snippet once and drop it into any reply — with the ticket number and requester name filled in automatically."
        />
      )}

      {replies.length > 0 && (
        <ul className="space-y-2">
          {replies.map((reply) => (
            <li
              key={reply.id}
              className="flex items-start justify-between gap-3 rounded-lg border p-3"
            >
              <div className="min-w-0">
                <div className="font-medium">{reply.title}</div>
                <p className="mt-0.5 line-clamp-2 whitespace-pre-wrap text-sm text-muted-foreground">
                  {reply.body}
                </p>
              </div>
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button
                    variant="ghost"
                    size="icon-xs"
                    aria-label={`Actions for ${reply.title}`}
                  >
                    <Ellipsis aria-hidden="true" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onSelect={() => setEditing(reply)}>
                    Edit
                  </DropdownMenuItem>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem
                    variant="destructive"
                    onSelect={() => setDeleting(reply)}
                  >
                    Delete
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </li>
          ))}
        </ul>
      )}

      <CannedReplyDialog open={createOpen} onOpenChange={setCreateOpen} />
      <CannedReplyDialog
        open={editing !== null}
        onOpenChange={(open) => !open && setEditing(null)}
        reply={editing ?? undefined}
      />

      <AlertDialog
        open={deleting !== null}
        onOpenChange={(open) => !open && setDeleting(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete “{deleting?.title}”?</AlertDialogTitle>
            <AlertDialogDescription>
              The snippet is removed for everyone. This cannot be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (deleting) deleteReply.mutate(deleting.id);
              }}
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </PageContainer>
  );
}

function CannedReplyDialog({
  open,
  onOpenChange,
  reply,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  reply?: CannedReply;
}) {
  const isEdit = reply !== undefined;
  const create = useCreateCannedReply();
  const update = useUpdateCannedReply();

  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [initialised, setInitialised] = useState(false);

  if (open && !initialised) {
    setTitle(reply?.title ?? "");
    setBody(reply?.body ?? "");
    setError(null);
    setInitialised(true);
  }

  function close(next: boolean) {
    onOpenChange(next);
    if (!next) setInitialised(false);
  }

  async function submit() {
    setError(null);
    try {
      if (isEdit) {
        await update.mutateAsync({
          id: reply.id,
          body: { title: title.trim(), body },
        });
      } else {
        await create.mutateAsync({ title: title.trim(), body });
      }
      close(false);
    } catch (err) {
      setError(err);
    }
  }

  const pending = create.isPending || update.isPending;
  const canSubmit = title.trim().length > 0 && body.trim().length > 0;

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>
            {isEdit ? "Edit canned reply" : "New canned reply"}
          </DialogTitle>
          <DialogDescription>
            Placeholders like{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-xs">
              {"{{requester.name}}"}
            </code>{" "}
            are filled in when the reply is inserted.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4">
          {error != null && <ProblemAlert error={error} />}
          <div className="space-y-2">
            <Label htmlFor="canned-title">Title</Label>
            <Input
              id="canned-title"
              autoComplete="off"
              value={title}
              onChange={(e) => setTitle(e.target.value)}
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="canned-body">Body</Label>
            <Textarea
              id="canned-body"
              className="min-h-32"
              value={body}
              onChange={(e) => setBody(e.target.value)}
            />
            <p className="text-xs text-muted-foreground">
              Placeholders: {CANNED_PLACEHOLDERS.join(", ")}
            </p>
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => close(false)}>
            Cancel
          </Button>
          <Button onClick={() => void submit()} disabled={!canSubmit || pending}>
            {pending ? "Saving…" : isEdit ? "Save changes" : "Create reply"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
