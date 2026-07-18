import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";

import { useCreateTicket } from "@/api/tickets";
import type { TicketPriority } from "@/api/types";
import { PRIORITY_LABELS, TICKET_PRIORITIES } from "@/api/types";
import { ProblemAlert } from "@/components/problem-alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";

/**
 * Minimal agent-side ticket creation: subject + first message + priority.
 * The API records the body as an agent-authored public web article and
 * allocates the day's ticket number atomically.
 */
export function NewTicketDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const navigate = useNavigate();
  const createTicket = useCreateTicket();

  const [subject, setSubject] = useState("");
  const [body, setBody] = useState("");
  const [priority, setPriority] = useState<TicketPriority>("medium");

  function reset() {
    setSubject("");
    setBody("");
    setPriority("medium");
    createTicket.reset();
  }

  function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!subject.trim() || !body.trim() || createTicket.isPending) return;
    createTicket.mutate(
      { subject: subject.trim(), body: body.trim(), priority },
      {
        onSuccess: (ticket) => {
          onOpenChange(false);
          reset();
          void navigate({
            to: "/tickets/$ticketId",
            params: { ticketId: ticket.id },
            search: (prev) => prev,
          });
        },
      },
    );
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        onOpenChange(next);
        if (!next) reset();
      }}
    >
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>New ticket</DialogTitle>
          <DialogDescription>
            Opens as an agent-authored public message on the web channel.
          </DialogDescription>
        </DialogHeader>

        <form onSubmit={onSubmit} className="space-y-4">
          {createTicket.isError && <ProblemAlert error={createTicket.error} />}

          <div className="space-y-2">
            <Label htmlFor="new-ticket-subject">Subject</Label>
            <Input
              id="new-ticket-subject"
              value={subject}
              onChange={(e) => setSubject(e.target.value)}
              placeholder="Printer on floor 3 is jammed"
              autoFocus
              required
            />
          </div>

          <div className="space-y-2">
            <Label htmlFor="new-ticket-body">Message</Label>
            <Textarea
              id="new-ticket-body"
              value={body}
              onChange={(e) => setBody(e.target.value)}
              placeholder="Describe the issue…"
              className="min-h-28"
              required
            />
          </div>

          <div className="space-y-2">
            <Label>Priority</Label>
            <Select
              value={priority}
              onValueChange={(v) => setPriority(v as TicketPriority)}
            >
              <SelectTrigger size="sm" className="w-40">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {TICKET_PRIORITIES.map((p) => (
                  <SelectItem key={p} value={p}>
                    {PRIORITY_LABELS[p]}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
            >
              Cancel
            </Button>
            <Button
              type="submit"
              disabled={
                !subject.trim() || !body.trim() || createTicket.isPending
              }
            >
              {createTicket.isPending ? "Creating…" : "Create ticket"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
