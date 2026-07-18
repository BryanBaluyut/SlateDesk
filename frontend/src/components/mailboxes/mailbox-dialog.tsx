import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery } from "@tanstack/react-query";
import {
  ArrowLeft,
  Check,
  Copy,
  ExternalLink,
  KeyRound,
  Loader2,
  Mail,
  X,
} from "lucide-react";
import { useEffect, useState } from "react";
import { useForm } from "react-hook-form";
import { z } from "zod";

import { meQueryOptions } from "@/api/auth";
import {
  startGoogleOauth,
  testMailboxFetch,
  testMailboxSend,
  useCreateMailbox,
  useUpdateMailbox,
} from "@/api/mailboxes";
import type {
  CreateMailboxRequest,
  Mailbox,
  MailboxAuthKind,
  MailboxTestResult,
  MailTLSMode,
  UpdateMailboxRequest,
} from "@/api/types";
import { TLS_MODE_LABELS, TLS_MODES } from "@/api/types";
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
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from "@/components/ui/form";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";

/** localStorage key the mailboxes route writes when the Google OAuth
 * callback lands with ?connected=1 — storage events carry it to the
 * window that opened the consent popup. */
export const GOOGLE_CONNECTED_KEY = "slatedesk-google-connected";

/** The scope the client-credentials grant requests (mirrors the server's
 * internal/email/auth.go). Mailbox access itself is granted in Entra via
 * the Exchange Online application permissions + admin consent. */
const M365_SCOPE = "https://outlook.office365.com/.default";

const portSchema = z
  .string()
  .regex(/^\d+$/, "Digits only")
  .refine(
    (v) => {
      const n = Number(v);
      return n >= 1 && n <= 65535;
    },
    { message: "1–65535" },
  );

const mailboxSchema = z.object({
  name: z.string().min(1, "Name is required"),
  email_address: z.email("Enter a valid email address"),
  imap_host: z.string().min(1, "Required"),
  imap_port: portSchema,
  imap_tls_mode: z.enum(["tls", "starttls", "none"]),
  imap_username: z.string().min(1, "Required"),
  smtp_host: z.string().min(1, "Required"),
  smtp_port: portSchema,
  smtp_tls_mode: z.enum(["tls", "starttls", "none"]),
  smtp_username: z.string().min(1, "Required"),
  from_display_name: z.string(),
  signature: z.string(),
  auto_ack_enabled: z.boolean(),
  active: z.boolean(),
  // Credential fields are optional at the schema level: on edit, blank
  // means "keep the stored secret". Create (or an auth-kind change)
  // enforces the kind's required set manually at the step boundary.
  password: z.string(),
  tenant_id: z.string(),
  client_id: z.string(),
  client_secret: z.string(),
});

type MailboxValues = z.infer<typeof mailboxSchema>;

type Step = "kind" | "connection" | "identity" | "verify";

const CONNECTION_FIELDS = [
  "name",
  "email_address",
  "imap_host",
  "imap_port",
  "imap_tls_mode",
  "imap_username",
  "smtp_host",
  "smtp_port",
  "smtp_tls_mode",
  "smtp_username",
] as const;

const CRED_FIELDS: Record<
  MailboxAuthKind,
  readonly ("password" | "tenant_id" | "client_id" | "client_secret")[]
> = {
  basic: ["password"],
  oauth_m365: ["tenant_id", "client_id", "client_secret"],
  oauth_google: ["client_id", "client_secret"],
};

/** Sane server defaults per kind: 993/TLS in, 587/STARTTLS out. */
const KIND_PRESETS: Record<
  MailboxAuthKind,
  Pick<
    MailboxValues,
    | "imap_host"
    | "imap_port"
    | "imap_tls_mode"
    | "smtp_host"
    | "smtp_port"
    | "smtp_tls_mode"
  >
> = {
  basic: {
    imap_host: "",
    imap_port: "993",
    imap_tls_mode: "tls",
    smtp_host: "",
    smtp_port: "587",
    smtp_tls_mode: "starttls",
  },
  oauth_m365: {
    imap_host: "outlook.office365.com",
    imap_port: "993",
    imap_tls_mode: "tls",
    smtp_host: "smtp.office365.com",
    smtp_port: "587",
    smtp_tls_mode: "starttls",
  },
  oauth_google: {
    imap_host: "imap.gmail.com",
    imap_port: "993",
    imap_tls_mode: "tls",
    smtp_host: "smtp.gmail.com",
    smtp_port: "587",
    smtp_tls_mode: "starttls",
  },
};

const KIND_OPTIONS: {
  kind: MailboxAuthKind;
  title: string;
  guidance: string;
}[] = [
  {
    kind: "oauth_m365",
    title: "Microsoft 365",
    guidance:
      "Entra app registration with client credentials — no user sign-in involved.",
  },
  {
    kind: "oauth_google",
    title: "Google",
    guidance:
      "Google Cloud OAuth client plus a one-time account consent in a browser window.",
  },
  {
    kind: "basic",
    title: "IMAP/SMTP password",
    guidance: "Classic username and password, for any mail server that allows it.",
  },
];

function defaultsFor(mailbox: Mailbox | undefined): MailboxValues {
  if (!mailbox) {
    return {
      name: "",
      email_address: "",
      imap_host: "",
      imap_port: "993",
      imap_tls_mode: "tls",
      imap_username: "",
      smtp_host: "",
      smtp_port: "587",
      smtp_tls_mode: "starttls",
      smtp_username: "",
      from_display_name: "",
      signature: "",
      auto_ack_enabled: true,
      active: true,
      password: "",
      tenant_id: "",
      client_id: "",
      client_secret: "",
    };
  }
  return {
    name: mailbox.name,
    email_address: mailbox.email_address,
    imap_host: mailbox.imap_host,
    imap_port: String(mailbox.imap_port),
    imap_tls_mode: mailbox.imap_tls_mode,
    imap_username: mailbox.imap_username,
    smtp_host: mailbox.smtp_host,
    smtp_port: String(mailbox.smtp_port),
    smtp_tls_mode: mailbox.smtp_tls_mode,
    smtp_username: mailbox.smtp_username,
    from_display_name: mailbox.from_display_name,
    signature: mailbox.signature,
    auto_ack_enabled: mailbox.auto_ack_enabled,
    active: mailbox.active,
    // Credentials are never returned by the API: fields start blank and
    // blank means "keep what is stored".
    password: "",
    tenant_id: "",
    client_id: "",
    client_secret: "",
  };
}

const STEP_LABEL: Record<Step, string> = {
  kind: "Step 1 of 3 — account type",
  connection: "Step 2 of 3 — connection",
  identity: "Step 3 of 3 — identity",
  verify: "Connection check",
};

/**
 * Add/edit mailbox as a calm multi-step flow: auth kind → kind-specific
 * connection fields → identity (from name, signature, auto-ack). Saving
 * lands on a final check stage with live fetch/send tests (and, for
 * Google mailboxes, the account consent flow).
 */
export function MailboxDialog({
  open,
  onOpenChange,
  mailbox,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Present in edit mode; absent in create mode. */
  mailbox?: Mailbox;
}) {
  const isEdit = mailbox !== undefined;
  const createMailbox = useCreateMailbox();
  const updateMailbox = useUpdateMailbox();

  const [step, setStep] = useState<Step>("kind");
  const [kind, setKind] = useState<MailboxAuthKind | null>(null);
  const [error, setError] = useState<unknown>(null);
  /** The persisted row: the edit subject, or the create result — the id
   * the verify stage tests against. */
  const [savedMailbox, setSavedMailbox] = useState<Mailbox | null>(null);

  const form = useForm<MailboxValues>({
    resolver: zodResolver(mailboxSchema),
    defaultValues: defaultsFor(undefined),
  });

  // Re-seed everything each time the dialog opens for a (different) subject.
  useEffect(() => {
    if (!open) return;
    setStep("kind");
    setKind(mailbox?.auth_kind ?? null);
    setError(null);
    setSavedMailbox(mailbox ?? null);
    form.reset(defaultsFor(mailbox));
  }, [open, mailbox, form]);

  const pending = createMailbox.isPending || updateMailbox.isPending;
  /** Blank credentials mean "keep" only while the kind is unchanged. */
  const requireCreds = !isEdit || (kind !== null && kind !== mailbox.auth_kind);

  function pickKind(next: MailboxAuthKind) {
    if (kind === next) return;
    setKind(next);
    // Presets only in create mode — an existing mailbox's servers are
    // deliberate configuration, not ours to overwrite.
    if (!isEdit) {
      const preset = KIND_PRESETS[next];
      const values = form.getValues();
      form.reset({ ...values, ...preset }, { keepDirtyValues: false });
    }
  }

  /** Create mode nicety: the login usually is the address itself. */
  function fillUsernamesFromEmail() {
    const email = form.getValues("email_address");
    if (email === "") return;
    if (form.getValues("imap_username") === "") {
      form.setValue("imap_username", email);
    }
    if (form.getValues("smtp_username") === "") {
      form.setValue("smtp_username", email);
    }
  }

  async function nextFromConnection() {
    const ok = await form.trigger(CONNECTION_FIELDS);
    if (!ok || kind === null) return;
    if (requireCreds) {
      const missing = CRED_FIELDS[kind].filter(
        (field) => form.getValues(field).trim() === "",
      );
      if (missing.length > 0) {
        for (const field of missing) {
          form.setError(field, { type: "required", message: "Required" });
        }
        return;
      }
    }
    setStep("identity");
  }

  function credentialPatch(values: MailboxValues): Partial<UpdateMailboxRequest> {
    if (kind === null) return {};
    const patch: Partial<UpdateMailboxRequest> = {};
    for (const field of CRED_FIELDS[kind]) {
      const value = values[field].trim();
      if (value !== "") patch[field] = value;
    }
    return patch;
  }

  async function onSubmit(values: MailboxValues) {
    if (kind === null) return;
    setError(null);
    const config = {
      name: values.name,
      email_address: values.email_address,
      active: values.active,
      imap_host: values.imap_host,
      imap_port: Number(values.imap_port),
      imap_tls_mode: values.imap_tls_mode,
      imap_username: values.imap_username,
      smtp_host: values.smtp_host,
      smtp_port: Number(values.smtp_port),
      smtp_tls_mode: values.smtp_tls_mode,
      smtp_username: values.smtp_username,
      from_display_name: values.from_display_name,
      signature: values.signature,
      auto_ack_enabled: values.auto_ack_enabled,
    };
    try {
      let saved: Mailbox;
      if (isEdit) {
        const body: UpdateMailboxRequest = {
          ...config,
          auth_kind: kind,
          ...credentialPatch(values),
        };
        saved = await updateMailbox.mutateAsync({ id: mailbox.id, body });
      } else {
        let body: CreateMailboxRequest;
        switch (kind) {
          case "basic":
            body = { ...config, auth_kind: "basic", password: values.password };
            break;
          case "oauth_m365":
            body = {
              ...config,
              auth_kind: "oauth_m365",
              tenant_id: values.tenant_id,
              client_id: values.client_id,
              client_secret: values.client_secret,
            };
            break;
          case "oauth_google":
            body = {
              ...config,
              auth_kind: "oauth_google",
              client_id: values.client_id,
              client_secret: values.client_secret,
            };
            break;
        }
        saved = await createMailbox.mutateAsync(body);
      }
      setSavedMailbox(saved);
      setStep("verify");
    } catch (err) {
      setError(err);
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="flex max-h-[85svh] flex-col overflow-hidden sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>{isEdit ? "Edit mailbox" : "Add mailbox"}</DialogTitle>
          <DialogDescription>{STEP_LABEL[step]}</DialogDescription>
        </DialogHeader>

        <div className="-mx-1 min-h-0 flex-1 overflow-y-auto px-1">
          {error != null && (
            <div className="mb-3">
              <ProblemAlert error={error} />
            </div>
          )}

          {step === "kind" && (
            <div className="flex flex-col gap-2">
              {KIND_OPTIONS.map((option) => (
                <button
                  key={option.kind}
                  type="button"
                  onClick={() => pickKind(option.kind)}
                  aria-pressed={kind === option.kind}
                  className={cn(
                    "flex items-start gap-3 rounded-lg border p-3 text-left transition-colors",
                    "hover:bg-accent/50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                    kind === option.kind && "border-primary/50 bg-primary/5",
                  )}
                >
                  <div
                    className={cn(
                      "mt-0.5 flex size-7 shrink-0 items-center justify-center rounded-full border",
                      kind === option.kind
                        ? "border-primary/50 bg-primary/10 text-primary"
                        : "bg-muted/50 text-muted-foreground",
                    )}
                  >
                    {option.kind === "basic" ? (
                      <KeyRound className="size-3.5" aria-hidden="true" />
                    ) : (
                      <Mail className="size-3.5" aria-hidden="true" />
                    )}
                  </div>
                  <div className="min-w-0">
                    <div className="text-sm font-medium">{option.title}</div>
                    <div className="mt-0.5 text-xs text-muted-foreground">
                      {option.guidance}
                    </div>
                  </div>
                  {kind === option.kind && (
                    <Check
                      className="ml-auto size-4 shrink-0 text-primary"
                      aria-hidden="true"
                    />
                  )}
                </button>
              ))}
              {isEdit && (
                <p className="text-xs text-muted-foreground">
                  Switching the account type requires entering that type's
                  credentials on the next step.
                </p>
              )}
            </div>
          )}

          {(step === "connection" || step === "identity") && kind !== null && (
            <Form {...form}>
              <form
                id="mailbox-form"
                onSubmit={form.handleSubmit(onSubmit)}
                noValidate
              >
                <div className={cn("space-y-4", step !== "connection" && "hidden")}>
                  <div className="grid grid-cols-2 gap-4">
                    <FormField
                      control={form.control}
                      name="name"
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>Mailbox name</FormLabel>
                          <FormControl>
                            <Input placeholder="Support" autoComplete="off" {...field} />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name="email_address"
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>Email address</FormLabel>
                          <FormControl>
                            <Input
                              type="email"
                              placeholder="support@example.com"
                              autoComplete="off"
                              {...field}
                              onBlur={() => {
                                field.onBlur();
                                fillUsernamesFromEmail();
                              }}
                            />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                  </div>

                  {kind === "oauth_m365" && (
                    <M365CredentialFields
                      requireCreds={requireCreds}
                      currentClientId={mailbox?.oauth_client_id ?? null}
                      currentTenantId={mailbox?.oauth_tenant_id ?? null}
                      form={form}
                    />
                  )}
                  {kind === "oauth_google" && (
                    <GoogleCredentialFields
                      requireCreds={requireCreds}
                      currentClientId={mailbox?.oauth_client_id ?? null}
                      form={form}
                      savedMailbox={
                        isEdit && mailbox.auth_kind === "oauth_google"
                          ? mailbox
                          : null
                      }
                    />
                  )}
                  {kind === "basic" && (
                    <FormField
                      control={form.control}
                      name="password"
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>Password</FormLabel>
                          <FormControl>
                            <Input
                              type="password"
                              autoComplete="new-password"
                              placeholder={
                                requireCreds ? undefined : "Leave blank to keep current"
                              }
                              {...field}
                            />
                          </FormControl>
                          <FormDescription>
                            Used for both IMAP and SMTP. Encrypted at rest,
                            never shown again.
                          </FormDescription>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                  )}

                  <ServerFields form={form} />
                </div>

                <div className={cn("space-y-4", step !== "identity" && "hidden")}>
                  <FormField
                    control={form.control}
                    name="from_display_name"
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>From display name</FormLabel>
                        <FormControl>
                          <Input
                            placeholder={form.getValues("name") || "Support"}
                            autoComplete="off"
                            {...field}
                          />
                        </FormControl>
                        <FormDescription>
                          Shown as the sender of outbound mail. Blank uses the
                          mailbox name.
                        </FormDescription>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                  <FormField
                    control={form.control}
                    name="signature"
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>Signature</FormLabel>
                        <FormControl>
                          <Textarea
                            rows={4}
                            placeholder={"—\nThe support team"}
                            {...field}
                          />
                        </FormControl>
                        <FormDescription>
                          Appended to every outbound email. Blank for none.
                        </FormDescription>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                  <FormField
                    control={form.control}
                    name="auto_ack_enabled"
                    render={({ field }) => (
                      <FormItem className="flex flex-row items-center justify-between gap-4 rounded-lg border p-3">
                        <div>
                          <FormLabel>Auto-acknowledge new tickets</FormLabel>
                          <FormDescription>
                            Confirmation email with the ticket number on every
                            new email ticket. Loop-guarded: autoresponders and
                            bulk mail never get one.
                          </FormDescription>
                        </div>
                        <FormControl>
                          <Switch
                            checked={field.value}
                            onCheckedChange={field.onChange}
                          />
                        </FormControl>
                      </FormItem>
                    )}
                  />
                  {isEdit && (
                    <FormField
                      control={form.control}
                      name="active"
                      render={({ field }) => (
                        <FormItem className="flex flex-row items-center justify-between gap-4 rounded-lg border p-3">
                          <div>
                            <FormLabel>Active</FormLabel>
                            <FormDescription>
                              Inactive mailboxes are neither polled nor sent
                              through.
                            </FormDescription>
                          </div>
                          <FormControl>
                            <Switch
                              checked={field.value}
                              onCheckedChange={field.onChange}
                            />
                          </FormControl>
                        </FormItem>
                      )}
                    />
                  )}
                </div>
              </form>
            </Form>
          )}

          {step === "verify" && savedMailbox !== null && (
            <VerifyPanel mailbox={savedMailbox} />
          )}
        </div>

        <DialogFooter>
          {step === "kind" && (
            <>
              <Button
                type="button"
                variant="outline"
                onClick={() => onOpenChange(false)}
              >
                Cancel
              </Button>
              <Button
                type="button"
                disabled={kind === null}
                onClick={() => setStep("connection")}
              >
                Next
              </Button>
            </>
          )}
          {step === "connection" && (
            <>
              <Button
                type="button"
                variant="outline"
                onClick={() => setStep("kind")}
              >
                <ArrowLeft aria-hidden="true" />
                Back
              </Button>
              <Button type="button" onClick={() => void nextFromConnection()}>
                Next
              </Button>
            </>
          )}
          {step === "identity" && (
            <>
              <Button
                type="button"
                variant="outline"
                onClick={() => setStep("connection")}
              >
                <ArrowLeft aria-hidden="true" />
                Back
              </Button>
              <Button type="submit" form="mailbox-form" disabled={pending}>
                {pending
                  ? "Saving…"
                  : isEdit
                    ? "Save & check"
                    : "Create & check"}
              </Button>
            </>
          )}
          {step === "verify" && (
            <Button type="button" onClick={() => onOpenChange(false)}>
              Done
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

type MailboxForm = ReturnType<typeof useForm<MailboxValues>>;

function M365CredentialFields({
  form,
  requireCreds,
  currentTenantId,
  currentClientId,
}: {
  form: MailboxForm;
  requireCreds: boolean;
  currentTenantId: string | null;
  currentClientId: string | null;
}) {
  const keepPlaceholder = requireCreds ? undefined : "Leave blank to keep current";
  return (
    <div className="space-y-4 rounded-lg border p-3">
      <p className="text-xs text-muted-foreground">
        In Entra, give the app registration the{" "}
        <span className="font-medium text-foreground">
          Office 365 Exchange Online
        </span>{" "}
        application permissions{" "}
        <code className="font-mono">IMAP.AccessAsApp</code> and{" "}
        <code className="font-mono">SMTP.SendAsApp</code>, grant admin
        consent, and allow this mailbox via an application access policy.
        Token requests use this scope:
      </p>
      <CopyableValue value={M365_SCOPE} label="Admin-consent scope" />
      <div className="grid grid-cols-2 gap-4">
        <FormField
          control={form.control}
          name="tenant_id"
          render={({ field }) => (
            <FormItem>
              <FormLabel>Tenant ID</FormLabel>
              <FormControl>
                <Input
                  autoComplete="off"
                  placeholder={keepPlaceholder}
                  {...field}
                />
              </FormControl>
              {currentTenantId !== null && (
                <FormDescription className="truncate">
                  Current: {currentTenantId}
                </FormDescription>
              )}
              <FormMessage />
            </FormItem>
          )}
        />
        <FormField
          control={form.control}
          name="client_id"
          render={({ field }) => (
            <FormItem>
              <FormLabel>Client ID</FormLabel>
              <FormControl>
                <Input
                  autoComplete="off"
                  placeholder={keepPlaceholder}
                  {...field}
                />
              </FormControl>
              {currentClientId !== null && (
                <FormDescription className="truncate">
                  Current: {currentClientId}
                </FormDescription>
              )}
              <FormMessage />
            </FormItem>
          )}
        />
      </div>
      <FormField
        control={form.control}
        name="client_secret"
        render={({ field }) => (
          <FormItem>
            <FormLabel>Client secret</FormLabel>
            <FormControl>
              <Input
                type="password"
                autoComplete="new-password"
                placeholder={keepPlaceholder}
                {...field}
              />
            </FormControl>
            <FormDescription>
              Encrypted at rest, never shown again.
            </FormDescription>
            <FormMessage />
          </FormItem>
        )}
      />
    </div>
  );
}

function GoogleCredentialFields({
  form,
  requireCreds,
  currentClientId,
  savedMailbox,
}: {
  form: MailboxForm;
  requireCreds: boolean;
  currentClientId: string | null;
  /** Non-null only in edit mode on an existing oauth_google mailbox —
   * the connect flow needs a persisted mailbox id. */
  savedMailbox: Mailbox | null;
}) {
  const keepPlaceholder = requireCreds ? undefined : "Leave blank to keep current";
  return (
    <div className="space-y-4 rounded-lg border p-3">
      <p className="text-xs text-muted-foreground">
        Create an OAuth client (type "Web application") in Google Cloud and
        register{" "}
        <code className="font-mono break-all">
          {"{external URL}"}/api/v1/mailboxes/oauth/google/callback
        </code>{" "}
        as an authorized redirect URI. The account consent runs in a
        separate window{savedMailbox === null ? " after the mailbox is saved" : ""}.
      </p>
      <div className="grid grid-cols-2 gap-4">
        <FormField
          control={form.control}
          name="client_id"
          render={({ field }) => (
            <FormItem>
              <FormLabel>Client ID</FormLabel>
              <FormControl>
                <Input
                  autoComplete="off"
                  placeholder={keepPlaceholder}
                  {...field}
                />
              </FormControl>
              {currentClientId !== null && (
                <FormDescription className="truncate">
                  Current: {currentClientId}
                </FormDescription>
              )}
              <FormMessage />
            </FormItem>
          )}
        />
        <FormField
          control={form.control}
          name="client_secret"
          render={({ field }) => (
            <FormItem>
              <FormLabel>Client secret</FormLabel>
              <FormControl>
                <Input
                  type="password"
                  autoComplete="new-password"
                  placeholder={keepPlaceholder}
                  {...field}
                />
              </FormControl>
              <FormMessage />
            </FormItem>
          )}
        />
      </div>
      {savedMailbox !== null && <GoogleConnectSection mailboxId={savedMailbox.id} />}
    </div>
  );
}

/** Compact IMAP/SMTP server grids shared by every kind. */
function ServerFields({ form }: { form: MailboxForm }) {
  return (
    <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
      {(["imap", "smtp"] as const).map((proto) => (
        <div key={proto} className="space-y-3 rounded-lg border p-3">
          <div className="text-xs font-medium tracking-wide text-muted-foreground uppercase">
            {proto === "imap" ? "IMAP — inbound" : "SMTP — outbound"}
          </div>
          <FormField
            control={form.control}
            name={`${proto}_host`}
            render={({ field }) => (
              <FormItem>
                <FormLabel>Host</FormLabel>
                <FormControl>
                  <Input autoComplete="off" {...field} />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
          <div className="grid grid-cols-[5rem_1fr] gap-3">
            <FormField
              control={form.control}
              name={`${proto}_port`}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Port</FormLabel>
                  <FormControl>
                    <Input inputMode="numeric" autoComplete="off" {...field} />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name={`${proto}_tls_mode`}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>TLS</FormLabel>
                  <Select
                    value={field.value}
                    onValueChange={(v) => field.onChange(v as MailTLSMode)}
                  >
                    <FormControl>
                      <SelectTrigger className="w-full">
                        <SelectValue />
                      </SelectTrigger>
                    </FormControl>
                    <SelectContent>
                      {TLS_MODES.map((mode) => (
                        <SelectItem key={mode} value={mode}>
                          {TLS_MODE_LABELS[mode]}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <FormMessage />
                </FormItem>
              )}
            />
          </div>
          <FormField
            control={form.control}
            name={`${proto}_username`}
            render={({ field }) => (
              <FormItem>
                <FormLabel>Username</FormLabel>
                <FormControl>
                  <Input autoComplete="off" {...field} />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
        </div>
      ))}
    </div>
  );
}

function CopyableValue({ value, label }: { value: string; label: string }) {
  const [copied, setCopied] = useState(false);
  function copy() {
    void navigator.clipboard.writeText(value).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  }
  return (
    <div className="flex items-center gap-1.5 rounded-md border bg-muted/50 py-1 pr-1 pl-2">
      <code className="min-w-0 flex-1 truncate font-mono text-xs" title={value}>
        {value}
      </code>
      <Button
        type="button"
        variant="ghost"
        size="icon-xs"
        onClick={copy}
        aria-label={`Copy ${label}`}
      >
        {copied ? (
          <Check className="text-emerald-500" aria-hidden="true" />
        ) : (
          <Copy aria-hidden="true" />
        )}
      </Button>
    </div>
  );
}

/**
 * "Connect Google account": asks the server for the authorization URL and
 * opens it in a popup window. The OAuth callback redirects that window to
 * /settings/mailboxes?connected=1, whose route broadcasts success through
 * localStorage — the storage event flips this section to "Connected".
 */
function GoogleConnectSection({ mailboxId }: { mailboxId: string }) {
  const [connected, setConnected] = useState(false);
  const start = useMutation({
    mutationFn: () => startGoogleOauth(mailboxId),
    onSuccess: ({ authorization_url }) => {
      window.open(authorization_url, "_blank", "popup,width=520,height=680");
    },
  });

  useEffect(() => {
    function onStorage(e: StorageEvent) {
      if (e.key === GOOGLE_CONNECTED_KEY && e.newValue !== null) {
        setConnected(true);
      }
    }
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, []);

  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2">
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={start.isPending}
          onClick={() => start.mutate()}
        >
          {start.isPending ? (
            <Loader2 className="animate-spin" aria-hidden="true" />
          ) : (
            <ExternalLink aria-hidden="true" />
          )}
          Connect Google account
        </Button>
        {connected && (
          <span className="flex items-center gap-1 text-sm text-emerald-600 dark:text-emerald-400">
            <Check className="size-3.5" aria-hidden="true" />
            Connected
          </span>
        )}
      </div>
      <p className="text-xs text-muted-foreground">
        Grants offline mailbox access; the refresh token is stored
        encrypted. Re-run after rotating the OAuth client.
      </p>
      {start.isError && <ProblemAlert error={start.error} />}
    </div>
  );
}

/** One live test row: run button + inline ok/fail result with latency. */
function TestRow({
  label,
  buttonLabel,
  mutation,
  onRun,
  children,
}: {
  label: string;
  buttonLabel: string;
  mutation: { isPending: boolean; data?: MailboxTestResult; isError: boolean; error: unknown };
  onRun: () => void;
  children?: React.ReactNode;
}) {
  return (
    <div className="space-y-2 rounded-lg border p-3">
      <div className="flex items-center gap-2">
        <span className="flex-1 text-sm font-medium">{label}</span>
        {children}
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={mutation.isPending}
          onClick={onRun}
        >
          {mutation.isPending && (
            <Loader2 className="animate-spin" aria-hidden="true" />
          )}
          {mutation.isPending ? "Testing…" : buttonLabel}
        </Button>
      </div>
      {mutation.data && (
        <div
          className={cn(
            "flex items-start gap-2 text-sm",
            mutation.data.ok
              ? "text-foreground/80"
              : "text-red-600 dark:text-red-400",
          )}
        >
          {mutation.data.ok ? (
            <Check
              className="mt-0.5 size-3.5 shrink-0 text-emerald-500"
              aria-hidden="true"
            />
          ) : (
            <X className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
          )}
          <span className="min-w-0 flex-1 break-words">
            {mutation.data.detail}
          </span>
          <span className="shrink-0 text-xs text-muted-foreground">
            {mutation.data.latency_ms} ms
          </span>
        </div>
      )}
      {mutation.isError && <ProblemAlert error={mutation.error} />}
    </div>
  );
}

/**
 * Post-save stage: live IMAP/SMTP tests against the stored credentials
 * (the API runs them server-side; failures come back as ok=false with the
 * failing step). Google mailboxes get the consent flow here too.
 */
function VerifyPanel({ mailbox }: { mailbox: Mailbox }) {
  const meQuery = useQuery(meQueryOptions);
  const [sendTo, setSendTo] = useState("");

  // Default the test recipient to the signed-in admin once known.
  const myEmail = meQuery.data?.email;
  useEffect(() => {
    if (myEmail !== undefined) {
      setSendTo((current) => (current === "" ? myEmail : current));
    }
  }, [myEmail]);

  const fetchTest = useMutation({
    mutationFn: () => testMailboxFetch(mailbox.id),
  });
  const sendTest = useMutation({
    mutationFn: (to: string) => testMailboxSend(mailbox.id, to),
  });

  return (
    <div className="space-y-3">
      <p className="text-sm text-muted-foreground">
        {mailbox.name} is saved. Prove the connection both ways — tests run
        live against the mail server and may take a few seconds.
      </p>

      {mailbox.auth_kind === "oauth_google" && (
        <div className="rounded-lg border p-3">
          <GoogleConnectSection mailboxId={mailbox.id} />
        </div>
      )}

      <TestRow
        label="Fetch (IMAP)"
        buttonLabel="Fetch test"
        mutation={fetchTest}
        onRun={() => fetchTest.mutate()}
      />

      <TestRow
        label="Send (SMTP)"
        buttonLabel="Send test"
        mutation={sendTest}
        onRun={() => {
          if (sendTo.trim() !== "") sendTest.mutate(sendTo.trim());
        }}
      >
        <Label htmlFor="test-send-to" className="sr-only">
          Send test to
        </Label>
        <Input
          id="test-send-to"
          type="email"
          value={sendTo}
          onChange={(e) => setSendTo(e.target.value)}
          placeholder="you@example.com"
          autoComplete="off"
          className="h-8 w-52"
        />
      </TestRow>
    </div>
  );
}
