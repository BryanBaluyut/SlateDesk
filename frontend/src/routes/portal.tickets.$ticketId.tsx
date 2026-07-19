import {
  useMutation,
  useQueryClient,
  useSuspenseQuery,
} from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { ArrowLeft } from "lucide-react";
import { useState } from "react";

import {
  portalTicketQueryOptions,
  portalTicketsQueryOptions,
  replyPortalTicket,
} from "@/api/portal";
import type { PortalArticle } from "@/api/types";
import { STATUS_LABELS } from "@/api/types";
import { ProblemAlert } from "@/components/problem-alert";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { formatThreadTime } from "@/lib/time";
import { cn } from "@/lib/utils";

export const Route = createFileRoute("/portal/tickets/$ticketId")({
  loader: ({ context, params }) =>
    context.queryClient.ensureQueryData(
      portalTicketQueryOptions(params.ticketId),
    ),
  component: PortalTicketDetail,
});

function PortalTicketDetail() {
  const { ticketId } = Route.useParams();
  const query = portalTicketQueryOptions(ticketId);
  const { data: ticket } = useSuspenseQuery(query);
  const closed = ticket.status === "closed";

  return (
    <div className="space-y-5">
      <Link
        to="/portal"
        className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground"
      >
        <ArrowLeft className="size-4" aria-hidden="true" />
        Back to my requests
      </Link>

      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="text-xl font-semibold tracking-tight">
            {ticket.subject}
          </h1>
          <p className="mt-1 text-xs text-muted-foreground">{ticket.number}</p>
        </div>
        <span
          className={
            "shrink-0 rounded-full px-2.5 py-0.5 text-xs font-medium " +
            (closed
              ? "bg-muted text-muted-foreground"
              : "bg-primary/10 text-primary")
          }
        >
          {STATUS_LABELS[ticket.status]}
        </span>
      </div>

      <ol className="space-y-3">
        {ticket.articles.map((a) => (
          <Message key={a.id} article={a} />
        ))}
      </ol>

      <ReplyBox ticketId={ticketId} />
    </div>
  );
}

function Message({ article }: { article: PortalArticle }) {
  const mine = article.sender_type === "customer";
  const who =
    article.sender_type === "customer"
      ? "You"
      : article.sender_type === "system"
        ? "SlateDesk"
        : "Support";
  return (
    <li
      className={cn(
        "rounded-xl border p-4",
        mine ? "bg-primary/5" : "bg-background",
      )}
    >
      <div className="mb-1.5 flex items-center justify-between gap-2">
        <span className="text-sm font-medium">{who}</span>
        <span className="text-xs text-muted-foreground">
          {formatThreadTime(article.created_at)}
        </span>
      </div>
      <div className="text-sm whitespace-pre-wrap break-words text-foreground/90">
        {article.body_text}
      </div>
    </li>
  );
}

function ReplyBox({ ticketId }: { ticketId: string }) {
  const queryClient = useQueryClient();
  const [body, setBody] = useState("");
  const mutation = useMutation({
    mutationFn: () => replyPortalTicket(ticketId, { body }),
    onSuccess: async () => {
      setBody("");
      // Refetch the thread and the list — a customer reply can flip a
      // waiting ticket back to open.
      await Promise.all([
        queryClient.invalidateQueries(portalTicketQueryOptions(ticketId)),
        queryClient.invalidateQueries(portalTicketsQueryOptions),
      ]);
    },
  });

  function submit(e: React.FormEvent) {
    e.preventDefault();
    if (body.trim() === "") return;
    mutation.mutate();
  }

  return (
    <form
      onSubmit={submit}
      className="space-y-3 rounded-xl border bg-background p-4"
    >
      <span className="text-sm font-medium">Add a reply</span>
      {mutation.isError && <ProblemAlert error={mutation.error} />}
      <Textarea
        value={body}
        onChange={(e) => setBody(e.target.value)}
        rows={4}
        placeholder="Write your reply…"
      />
      <div className="flex justify-end">
        <Button type="submit" disabled={mutation.isPending || body.trim() === ""}>
          {mutation.isPending ? "Sending…" : "Send reply"}
        </Button>
      </div>
    </form>
  );
}
