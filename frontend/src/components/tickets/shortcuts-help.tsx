import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

const GROUPS: { heading: string; rows: [string, string][] }[] = [
  {
    heading: "Queue",
    rows: [
      ["j / k", "Move selection down / up"],
      ["Enter or o", "Open selected ticket"],
      ["/", "Focus search"],
    ],
  },
  {
    heading: "Ticket",
    rows: [
      ["r", "Reply (focus composer)"],
      ["n", "Internal note (focus composer)"],
      ["⌘/Ctrl + Enter", "Send message"],
      ["a", "Assign…"],
      ["s", "Set status…"],
      ["p", "Set priority…"],
      ["x", "Toggle context sidebar"],
    ],
  },
  {
    heading: "Everywhere",
    rows: [
      ["⌘/Ctrl + K", "Command palette"],
      ["?", "This help"],
    ],
  },
];

export function ShortcutsHelpDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Keyboard shortcuts</DialogTitle>
          <DialogDescription>
            The workspace is keyboard-first; shortcuts are disabled while
            typing.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4">
          {GROUPS.map((group) => (
            <div key={group.heading}>
              <h3 className="mb-1.5 text-xs font-medium text-muted-foreground uppercase tracking-wide">
                {group.heading}
              </h3>
              <dl className="space-y-1">
                {group.rows.map(([keys, description]) => (
                  <div
                    key={keys}
                    className="flex items-center justify-between gap-4 text-sm"
                  >
                    <dt className="text-muted-foreground">{description}</dt>
                    <dd className="shrink-0">
                      <kbd className="rounded border bg-muted px-1.5 py-0.5 font-mono text-[11px]">
                        {keys}
                      </kbd>
                    </dd>
                  </div>
                ))}
              </dl>
            </div>
          ))}
        </div>
      </DialogContent>
    </Dialog>
  );
}
