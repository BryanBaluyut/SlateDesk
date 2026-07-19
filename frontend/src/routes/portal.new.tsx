import { useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link, useRouter } from "@tanstack/react-router";
import { ArrowLeft } from "lucide-react";
import { useState } from "react";

import { createPortalTicket, portalTicketsQueryOptions } from "@/api/portal";
import { ProblemAlert } from "@/components/problem-alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";

export const Route = createFileRoute("/portal/new")({
  component: NewRequest,
});

function NewRequest() {
  const router = useRouter();
  const queryClient = useQueryClient();
  const [subject, setSubject] = useState("");
  const [body, setBody] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      const ticket = await createPortalTicket({ subject, body });
      await queryClient.invalidateQueries(portalTicketsQueryOptions);
      router.navigate({
        to: "/portal/tickets/$ticketId",
        params: { ticketId: ticket.id },
      });
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  }

  return (
    <div className="space-y-5">
      <Link
        to="/portal"
        className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground"
      >
        <ArrowLeft className="size-4" aria-hidden="true" />
        Back to my requests
      </Link>

      <div>
        <h1 className="text-xl font-semibold tracking-tight">New request</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          Tell us what you need help with.
        </p>
      </div>

      <form
        onSubmit={submit}
        className="space-y-4 rounded-xl border bg-background p-6"
        noValidate
      >
        {error != null && <ProblemAlert error={error} />}
        <div className="space-y-1.5">
          <Label>Subject</Label>
          <Input
            value={subject}
            onChange={(e) => setSubject(e.target.value)}
            autoFocus
            required
          />
        </div>
        <div className="space-y-1.5">
          <Label>Details</Label>
          <Textarea
            value={body}
            onChange={(e) => setBody(e.target.value)}
            rows={7}
            required
          />
        </div>
        <div className="flex justify-end gap-2">
          <Button asChild variant="ghost" type="button">
            <Link to="/portal">Cancel</Link>
          </Button>
          <Button type="submit" disabled={busy}>
            {busy ? "Submitting…" : "Submit request"}
          </Button>
        </div>
      </form>
    </div>
  );
}
