import { useQueryClient } from "@tanstack/react-query";
import { createFileRoute, useRouter } from "@tanstack/react-router";
import { useState } from "react";

import { meQueryOptions } from "@/api/auth";
import {
  completeSetup,
  createSetupAdmin,
  getSetupStatus,
  setSetupInstance,
} from "@/api/setup";
import { ProblemAlert } from "@/components/problem-alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

/** Installer wizard, outside the auth guard. `?token=` comes from the logs. */
export const Route = createFileRoute("/setup")({
  validateSearch: (search: Record<string, unknown>) => ({
    token: typeof search.token === "string" ? search.token : "",
  }),
  loader: () => getSetupStatus(),
  component: SetupWizard,
});

type Step = "admin" | "instance" | "mailbox";

function SetupWizard() {
  const status = Route.useLoaderData();
  const { token } = Route.useSearch();
  const router = useRouter();
  const queryClient = useQueryClient();

  // If setup already finished, there is nothing to do here.
  const [done] = useState(status.completed);
  // Resume the wizard where it actually is: needs_token means no admin yet
  // (start at "admin"); needs_token false with setup incomplete means the
  // admin was already created (its session is live) but the wizard was
  // reloaded mid-flow — resume at "instance" rather than trapping the operator
  // on the admin step, whose form now 409s.
  const [step, setStep] = useState<Step>(
    status.completed ? "mailbox" : status.needs_token ? "admin" : "instance",
  );

  if (done) {
    return (
      <Shell step={3}>
        <h1 className="text-lg font-semibold tracking-tight">
          Setup already complete
        </h1>
        <p className="mt-2 text-sm text-muted-foreground">
          This SlateDesk instance is already set up.
        </p>
        <Button className="mt-6 w-full" onClick={() => router.navigate({ to: "/" })}>
          Go to SlateDesk
        </Button>
      </Shell>
    );
  }

  async function finish() {
    const res = await completeSetup();
    // The admin session was set by createSetupAdmin; land in the workspace.
    queryClient.removeQueries({ queryKey: ["auth"] });
    await queryClient.ensureQueryData(meQueryOptions);
    router.navigate({ to: "/tickets" });
    return res;
  }

  return (
    <Shell step={step === "admin" ? 1 : step === "instance" ? 2 : 3}>
      {step === "admin" && (
        <AdminStep
          token={token}
          needsToken={status.needs_token}
          onDone={() => setStep("instance")}
        />
      )}
      {step === "instance" && (
        <InstanceStep onDone={() => setStep("mailbox")} />
      )}
      {step === "mailbox" && <MailboxStep onFinish={finish} />}
    </Shell>
  );
}

function Shell({ step, children }: { step: number; children: React.ReactNode }) {
  return (
    <div className="grid min-h-svh place-items-center bg-background px-4 py-10">
      <div className="w-full max-w-md">
        <div className="mb-6 flex flex-col items-center gap-3">
          <div className="flex size-9 items-center justify-center rounded-lg bg-primary text-base font-semibold text-primary-foreground">
            S
          </div>
          <p className="text-xs font-medium text-muted-foreground">
            Set up SlateDesk · Step {step} of 3
          </p>
          <div className="flex gap-1.5" aria-hidden="true">
            {[1, 2, 3].map((n) => (
              <span
                key={n}
                className={
                  "h-1.5 w-8 rounded-full " +
                  (n <= step ? "bg-primary" : "bg-muted")
                }
              />
            ))}
          </div>
        </div>
        <div className="rounded-xl border bg-card p-6 shadow-xs">{children}</div>
      </div>
    </div>
  );
}

function AdminStep({
  token,
  needsToken,
  onDone,
}: {
  token: string;
  needsToken: boolean;
  onDone: () => void;
}) {
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);

  const missingToken = needsToken && token === "";

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      await createSetupAdmin({ token, name, email, password });
      onDone();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit} className="space-y-4" noValidate>
      <div>
        <h1 className="text-lg font-semibold tracking-tight">
          Create your admin account
        </h1>
        <p className="mt-1 text-sm text-muted-foreground">
          This is the first administrator for your help desk.
        </p>
      </div>
      {missingToken && (
        <p className="rounded-md border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-sm text-amber-700 dark:text-amber-400">
          Open this page using the installer link printed in the server logs —
          it carries the one-time setup token.
        </p>
      )}
      {error != null && <ProblemAlert error={error} />}
      <Field label="Your name">
        <Input value={name} onChange={(e) => setName(e.target.value)} autoFocus required />
      </Field>
      <Field label="Email">
        <Input
          type="email"
          autoComplete="email"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          required
        />
      </Field>
      <Field label="Password" hint="At least 10 characters.">
        <Input
          type="password"
          autoComplete="new-password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          required
        />
      </Field>
      <Button type="submit" className="w-full" disabled={busy || missingToken}>
        {busy ? "Creating…" : "Create admin & continue"}
      </Button>
    </form>
  );
}

function InstanceStep({ onDone }: { onDone: () => void }) {
  const [name, setName] = useState("");
  const [externalUrl, setExternalUrl] = useState(window.location.origin);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      await setSetupInstance({ name, external_url: externalUrl });
      onDone();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit} className="space-y-4" noValidate>
      <div>
        <h1 className="text-lg font-semibold tracking-tight">Name your instance</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          How your help desk is branded, and its public address.
        </p>
      </div>
      {error != null && <ProblemAlert error={error} />}
      <Field label="Instance name">
        <Input
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="Acme Support"
          autoFocus
          required
        />
      </Field>
      <Field label="External URL" hint="Where users reach this instance.">
        <Input
          type="url"
          value={externalUrl}
          onChange={(e) => setExternalUrl(e.target.value)}
          placeholder="https://desk.example.com"
          required
        />
      </Field>
      <Button type="submit" className="w-full" disabled={busy}>
        {busy ? "Saving…" : "Save & continue"}
      </Button>
    </form>
  );
}

function MailboxStep({ onFinish }: { onFinish: () => Promise<unknown> }) {
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);

  async function finish() {
    setError(null);
    setBusy(true);
    try {
      await onFinish();
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  }

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-lg font-semibold tracking-tight">Connect a mailbox</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          Connect an email mailbox so tickets can arrive by email. You can do
          this now from <span className="font-medium">Settings → Mailboxes</span>,
          or skip and set it up later — the API, public form, and portal work
          immediately.
        </p>
      </div>
      {error != null && <ProblemAlert error={error} />}
      <Button className="w-full" onClick={finish} disabled={busy}>
        {busy ? "Finishing…" : "Finish setup"}
      </Button>
      <p className="text-center text-xs text-muted-foreground">
        A sample welcome ticket will be waiting in your workspace.
      </p>
    </div>
  );
}

function Field({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="space-y-1.5">
      <Label>{label}</Label>
      {children}
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  );
}
