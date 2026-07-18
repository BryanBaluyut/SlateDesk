import { useQueryClient } from "@tanstack/react-query";
import { Paperclip, SendHorizontal, X } from "lucide-react";
import { useCallback, useRef, useState } from "react";

import { MAX_ATTACHMENT_BYTES, uploadAttachment } from "@/api/attachments";
import { isApiError } from "@/api/client";
import { useCreateArticle } from "@/api/tickets";
import type { TicketDetail } from "@/api/types";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { useShortcut } from "@/lib/shortcuts";
import { formatBytes } from "@/lib/time";
import { cn } from "@/lib/utils";

type ComposerMode = "reply" | "note";

interface PendingFile {
  file: File;
  /** 0..1 while uploading; 1 done; null not started. */
  progress: number | null;
}

/**
 * Docked composer: segmented Reply | Internal-note toggle (amber surface in
 * note mode), plain textarea, attachments uploaded after article create with
 * visible progress. Cmd/Ctrl+Enter sends; r / n focus in the given mode.
 */
export function Composer({ ticket }: { ticket: TicketDetail }) {
  const queryClient = useQueryClient();
  const createArticle = useCreateArticle(ticket.id);

  const [mode, setMode] = useState<ComposerMode>("reply");
  const [body, setBody] = useState("");
  const [files, setFiles] = useState<PendingFile[]>([]);
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  const focusAs = useCallback((next: ComposerMode) => {
    setMode(next);
    // Focus after the mode swap paints.
    requestAnimationFrame(() => textareaRef.current?.focus());
  }, []);

  useShortcut("reply", useCallback(() => focusAs("reply"), [focusAs]));
  useShortcut("note", useCallback(() => focusAs("note"), [focusAs]));

  function addFiles(list: FileList | null) {
    if (!list) return;
    const additions: PendingFile[] = [];
    for (const file of Array.from(list)) {
      if (file.size > MAX_ATTACHMENT_BYTES) {
        setError(`${file.name} exceeds the 25 MiB attachment limit.`);
        continue;
      }
      additions.push({ file, progress: null });
    }
    if (additions.length > 0) {
      setFiles((prev) => [...prev, ...additions]);
    }
  }

  async function send() {
    const text = body.trim();
    if (!text || sending) return;
    setSending(true);
    setError(null);
    // Keep focus in the (now read-only) textarea for the whole round-trip:
    // if focus fell to <body> — e.g. the Send button disables itself —
    // keystrokes typed while waiting would fire the global single-key
    // shortcuts (s opens Status, a opens Assignee, …).
    textareaRef.current?.focus();
    try {
      const article = await createArticle.mutateAsync({
        body_text: text,
        is_internal: mode === "note",
      });

      // Attachments upload after the article exists, one at a time, with
      // per-file progress. The article is already posted at this point, so a
      // failed upload is reported (with what was skipped) rather than
      // retried into a duplicate message.
      const queue = files;
      for (let i = 0; i < queue.length; i++) {
        const pending = queue[i];
        if (!pending) continue;
        try {
          await uploadAttachment(article.id, pending.file, (fraction) => {
            setFiles((prev) =>
              prev.map((f, idx) =>
                idx === i ? { ...f, progress: fraction } : f,
              ),
            );
          });
        } catch (err) {
          const detail = isApiError(err) ? err.message : String(err);
          const skipped = queue
            .slice(i + 1)
            .map((f) => f.file.name)
            .join(", ");
          setBody("");
          setFiles([]);
          setError(
            `Message sent, but uploading ${pending.file.name} failed: ${detail}.` +
              (skipped ? ` Not attached: ${skipped}.` : ""),
          );
          return;
        }
      }

      setBody("");
      setFiles([]);
    } catch (err) {
      setError(isApiError(err) ? err.message : "Sending failed. Try again.");
    } finally {
      setSending(false);
      void queryClient.invalidateQueries({
        queryKey: ["tickets", "detail", ticket.id],
      });
      void queryClient.invalidateQueries({ queryKey: ["tickets", "list"] });
      void queryClient.invalidateQueries({ queryKey: ["dashboard"] });
    }
  }

  function onKeyDown(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      void send();
    }
    if (e.key === "Escape") {
      e.currentTarget.blur();
    }
  }

  const isNote = mode === "note";
  const canSend = body.trim().length > 0 && !sending;

  return (
    <div
      className={cn(
        "shrink-0 border-t p-3 transition-colors",
        isNote && "bg-amber-500/10 dark:bg-amber-400/10",
      )}
    >
      <div className="mx-auto max-w-3xl">
        {/* Segmented mode toggle. Plain aria-pressed toggle buttons, NOT
            role=tab: these are ordinary Tab-reachable buttons without the
            tabs pattern's arrow-key/roving-tabindex behavior, so tab
            semantics would announce an interaction model that lies. */}
        <div className="mb-2 flex items-center gap-2">
          <div
            role="group"
            aria-label="Composer mode"
            className="inline-flex rounded-md border p-0.5"
          >
            <button
              type="button"
              aria-pressed={!isNote}
              onClick={() => focusAs("reply")}
              className={cn(
                "rounded px-2.5 py-1 text-xs font-medium transition-colors",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                !isNote
                  ? "bg-accent text-accent-foreground"
                  : "text-muted-foreground hover:text-foreground",
              )}
            >
              Reply
            </button>
            <button
              type="button"
              aria-pressed={isNote}
              onClick={() => focusAs("note")}
              className={cn(
                "rounded px-2.5 py-1 text-xs font-medium transition-colors",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                isNote
                  ? "bg-amber-500/20 text-amber-800 dark:bg-amber-400/20 dark:text-amber-200"
                  : "text-muted-foreground hover:text-foreground",
              )}
            >
              Internal note
            </button>
          </div>
          {isNote && (
            <span className="text-xs text-amber-700 dark:text-amber-300">
              Never visible to the customer
            </span>
          )}
        </div>

        <Textarea
          ref={textareaRef}
          value={body}
          onChange={(e) => setBody(e.target.value)}
          onKeyDown={onKeyDown}
          // readOnly, not disabled: a disabled textarea drops focus to
          // <body>, where the next keystrokes trigger global shortcuts.
          readOnly={sending}
          aria-busy={sending}
          placeholder={
            isNote
              ? "Write an internal note…  (n)"
              : ticket.status === "closed"
                ? "Reply to this closed ticket…  (r)"
                : "Write a reply…  (r)"
          }
          aria-label={isNote ? "Internal note" : "Reply"}
          className={cn(
            "max-h-56 min-h-20",
            isNote &&
              "border-amber-500/40 bg-amber-500/5 focus-visible:border-amber-500/60 focus-visible:ring-amber-500/20 dark:border-amber-400/30 dark:bg-amber-400/5",
          )}
        />

        {/* Pending attachments */}
        {files.length > 0 && (
          <div className="mt-2 flex flex-wrap gap-1.5">
            {files.map((pending, index) => (
              <span
                key={`${pending.file.name}-${index}`}
                className="relative inline-flex max-w-60 items-center gap-1.5 overflow-hidden rounded-md border bg-background/60 px-2 py-1 text-xs"
              >
                {pending.progress !== null && (
                  <span
                    className="absolute inset-y-0 left-0 bg-primary/15 transition-[width]"
                    style={{ width: `${Math.round(pending.progress * 100)}%` }}
                    aria-hidden="true"
                  />
                )}
                <Paperclip
                  className="size-3 shrink-0 text-muted-foreground"
                  aria-hidden="true"
                />
                <span className="truncate">{pending.file.name}</span>
                <span className="shrink-0 text-muted-foreground">
                  {pending.progress !== null
                    ? `${Math.round(pending.progress * 100)}%`
                    : formatBytes(pending.file.size)}
                </span>
                {!sending && (
                  <button
                    type="button"
                    aria-label={`Remove ${pending.file.name}`}
                    onClick={() =>
                      setFiles((prev) => prev.filter((_, i) => i !== index))
                    }
                    className="shrink-0 rounded text-muted-foreground hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  >
                    <X className="size-3" aria-hidden="true" />
                  </button>
                )}
              </span>
            ))}
          </div>
        )}

        {error && (
          <p role="alert" className="mt-2 text-xs text-destructive">
            {error}
          </p>
        )}

        <div className="mt-2 flex items-center justify-between">
          <input
            ref={fileInputRef}
            type="file"
            multiple
            className="hidden"
            onChange={(e) => {
              addFiles(e.target.files);
              e.target.value = "";
            }}
          />
          <Button
            type="button"
            variant="ghost"
            size="icon-sm"
            aria-label="Attach files"
            title="Attach files"
            disabled={sending}
            onClick={() => fileInputRef.current?.click()}
          >
            <Paperclip aria-hidden="true" />
          </Button>

          <div className="flex items-center gap-2.5">
            <span className="hidden text-[11px] text-muted-foreground sm:block">
              <kbd className="rounded border bg-muted px-1 font-mono">
                ⌘/Ctrl
              </kbd>
              +
              <kbd className="rounded border bg-muted px-1 font-mono">↵</kbd>{" "}
              to send
            </span>
            <Button
              type="button"
              size="sm"
              onClick={() => void send()}
              disabled={!canSend}
              className={cn(
                // amber-700 (not 600) in light mode: white 14px text needs
                // >= 4.5:1 (WCAG AA); on amber-600 it is only ~3.2:1.
                isNote &&
                  "bg-amber-700 text-white hover:bg-amber-800 dark:bg-amber-500 dark:text-amber-950 dark:hover:bg-amber-500/90",
              )}
            >
              <SendHorizontal aria-hidden="true" />
              {sending
                ? "Sending…"
                : isNote
                  ? "Add note"
                  : "Send reply"}
            </Button>
          </div>
        </div>
      </div>
    </div>
  );
}
