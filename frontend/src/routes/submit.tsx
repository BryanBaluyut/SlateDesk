import { createFileRoute } from "@tanstack/react-router";
import { CheckCircle2 } from "lucide-react";
import { useState } from "react";

import { submitPublicTicket } from "@/api/public";
import { ProblemAlert } from "@/components/problem-alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";

/** The single built-in public web form. Unauthenticated. */
export const Route = createFileRoute("/submit")({
  component: PublicForm,
});

function PublicForm() {
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [subject, setSubject] = useState("");
  const [body, setBody] = useState("");
  // Hidden honeypot: real users never fill it; bots that do are dropped.
  const [honeypot, setHoneypot] = useState("");

  const [ticketNumber, setTicketNumber] = useState<string | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      const res = await submitPublicTicket({
        name,
        email,
        subject,
        body,
        _honeypot: honeypot,
      });
      // A dropped (honeypot) submission returns 202 without a number; show the
      // same success either way so a bot learns nothing.
      setTicketNumber(res.ticket_number ?? "");
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="grid min-h-svh place-items-center bg-background px-4 py-10">
      <div className="w-full max-w-lg">
        <div className="mb-6 flex flex-col items-center gap-3 text-center">
          <div className="flex size-9 items-center justify-center rounded-lg bg-primary text-base font-semibold text-primary-foreground">
            S
          </div>
          <div>
            <h1 className="text-lg font-semibold tracking-tight">
              Contact support
            </h1>
            <p className="text-sm text-muted-foreground">
              Send us a message and we&apos;ll get back to you by email.
            </p>
          </div>
        </div>

        <div className="rounded-xl border bg-card p-6 shadow-xs">
          {ticketNumber !== null ? (
            <div className="flex flex-col items-center gap-3 py-4 text-center">
              <CheckCircle2 className="size-8 text-emerald-500" aria-hidden="true" />
              <h2 className="text-base font-medium">Thanks — we got your message</h2>
              {ticketNumber ? (
                <p className="text-sm text-muted-foreground">
                  Your ticket number is{" "}
                  <span className="font-mono font-medium text-foreground">
                    {ticketNumber}
                  </span>
                  . We&apos;ll reply to your email.
                </p>
              ) : (
                <p className="text-sm text-muted-foreground">
                  We&apos;ll reply to your email shortly.
                </p>
              )}
              <Button
                variant="outline"
                className="mt-2"
                onClick={() => {
                  setTicketNumber(null);
                  setSubject("");
                  setBody("");
                }}
              >
                Submit another
              </Button>
            </div>
          ) : (
            <form onSubmit={submit} className="space-y-4" noValidate>
              {error != null && <ProblemAlert error={error} />}

              {/* Honeypot: visually hidden, off-screen, not tab-focusable. */}
              <div aria-hidden="true" className="hidden">
                <label>
                  Leave this field empty
                  <input
                    type="text"
                    tabIndex={-1}
                    autoComplete="off"
                    value={honeypot}
                    onChange={(e) => setHoneypot(e.target.value)}
                  />
                </label>
              </div>

              <div className="grid gap-4 sm:grid-cols-2">
                <div className="space-y-1.5">
                  <Label>Your name</Label>
                  <Input
                    value={name}
                    onChange={(e) => setName(e.target.value)}
                    placeholder="Jane Doe"
                  />
                </div>
                <div className="space-y-1.5">
                  <Label>Email</Label>
                  <Input
                    type="email"
                    autoComplete="email"
                    value={email}
                    onChange={(e) => setEmail(e.target.value)}
                    placeholder="you@example.com"
                    required
                  />
                </div>
              </div>
              <div className="space-y-1.5">
                <Label>Subject</Label>
                <Input
                  value={subject}
                  onChange={(e) => setSubject(e.target.value)}
                  required
                />
              </div>
              <div className="space-y-1.5">
                <Label>How can we help?</Label>
                <Textarea
                  value={body}
                  onChange={(e) => setBody(e.target.value)}
                  rows={6}
                  required
                />
              </div>
              <Button type="submit" className="w-full" disabled={busy}>
                {busy ? "Sending…" : "Send message"}
              </Button>
            </form>
          )}
        </div>
      </div>
    </div>
  );
}
