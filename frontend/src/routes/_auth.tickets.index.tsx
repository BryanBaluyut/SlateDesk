import { createFileRoute } from "@tanstack/react-router";
import { Inbox } from "lucide-react";

export const Route = createFileRoute("/_auth/tickets/")({
  component: NoTicketSelected,
});

/** Center-pane empty state when no ticket is open. */
function NoTicketSelected() {
  return (
    <div className="flex h-full flex-col items-center justify-center gap-3 p-8 text-center">
      <div className="flex size-10 items-center justify-center rounded-full border bg-muted/50">
        <Inbox className="size-5 text-muted-foreground" aria-hidden="true" />
      </div>
      <div>
        <h2 className="text-sm font-medium">No ticket selected</h2>
        <p className="mt-1 max-w-sm text-sm text-muted-foreground">
          Pick a ticket from the queue, or use{" "}
          <kbd className="rounded border bg-muted px-1 font-mono text-[11px]">j</kbd>
          /
          <kbd className="rounded border bg-muted px-1 font-mono text-[11px]">k</kbd>{" "}
          and{" "}
          <kbd className="rounded border bg-muted px-1 font-mono text-[11px]">
            Enter
          </kbd>
          . Press{" "}
          <kbd className="rounded border bg-muted px-1 font-mono text-[11px]">?</kbd>{" "}
          for all shortcuts.
        </p>
      </div>
    </div>
  );
}
