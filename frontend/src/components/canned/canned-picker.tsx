import { useQuery } from "@tanstack/react-query";
import { MessageSquareText } from "lucide-react";
import { useState } from "react";

import { cannedRepliesQueryOptions } from "@/api/cannedReplies";
import type { TicketDetail } from "@/api/types";
import { Button } from "@/components/ui/button";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import { renderCanned } from "@/lib/canned";

/**
 * Composer picker: search canned replies and insert one. Variable
 * placeholders ({{ticket.number}}, {{requester.name}}, …) are substituted
 * client-side against the open ticket at insert time (see lib/canned.ts).
 */
export function CannedPicker({
  ticket,
  onInsert,
  disabled,
}: {
  ticket: TicketDetail;
  onInsert: (text: string) => void;
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const query = useQuery({ ...cannedRepliesQueryOptions, enabled: open });
  const replies = query.data ?? [];

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          aria-label="Insert canned reply"
          title="Insert canned reply"
          disabled={disabled}
        >
          <MessageSquareText aria-hidden="true" />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-80 p-0">
        <Command>
          <CommandInput placeholder="Search canned replies…" />
          <CommandList>
            <CommandEmpty>
              {query.isPending ? "Loading…" : "No canned replies."}
            </CommandEmpty>
            <CommandGroup>
              {replies.map((reply) => (
                <CommandItem
                  key={reply.id}
                  value={`${reply.title} ${reply.body}`}
                  onSelect={() => {
                    onInsert(renderCanned(reply.body, ticket));
                    setOpen(false);
                  }}
                  className="flex-col items-start gap-0.5"
                >
                  <span className="truncate text-sm font-medium">
                    {reply.title}
                  </span>
                  <span className="line-clamp-2 text-xs text-muted-foreground">
                    {reply.body}
                  </span>
                </CommandItem>
              ))}
            </CommandGroup>
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}
