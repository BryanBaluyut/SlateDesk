import { zodResolver } from "@hookform/resolvers/zod";
import { useQuery } from "@tanstack/react-query";
import { Check, ChevronsUpDown, X } from "lucide-react";
import { useEffect, useState } from "react";
import { useForm } from "react-hook-form";
import { z } from "zod";

import { teamQueryOptions, useCreateTeam, useUpdateTeam } from "@/api/teams";
import type { Team, User } from "@/api/types";
import { usersQueryOptions } from "@/api/users";
import { ProblemAlert } from "@/components/problem-alert";
import { Badge } from "@/components/ui/badge";
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
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import { cn } from "@/lib/utils";

const teamSchema = z.object({
  name: z.string().min(1, "Name is required"),
  description: z.string(),
});

type TeamValues = z.infer<typeof teamSchema>;

/**
 * Create + edit share one dialog. Members are picked with a searchable
 * multi-select over active agents and admins (teams are routing queues, so
 * customers are not eligible). Membership is replaced wholesale via
 * PUT /teams/{id}/members on save.
 */
export function TeamDialog({
  open,
  onOpenChange,
  team,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Present in edit mode; absent in create mode. */
  team?: Team;
}) {
  const isEdit = team !== undefined;
  const createTeam = useCreateTeam();
  const updateTeam = useUpdateTeam();
  const [error, setError] = useState<unknown>(null);
  const [memberIds, setMemberIds] = useState<string[]>([]);
  const [pickerOpen, setPickerOpen] = useState(false);

  const usersQuery = useQuery({ ...usersQueryOptions, enabled: open });
  // The list endpoint has no members; fetch the detail in edit mode.
  const teamDetailQuery = useQuery({
    ...teamQueryOptions(team?.id ?? ""),
    enabled: open && isEdit,
  });

  const eligibleUsers = (usersQuery.data ?? []).filter(
    (u) => u.active && (u.role === "agent" || u.role === "admin"),
  );

  const form = useForm<TeamValues>({
    resolver: zodResolver(teamSchema),
    defaultValues: { name: "", description: "" },
  });

  // Seed the form when the dialog opens.
  useEffect(() => {
    if (!open) {
      return;
    }
    setError(null);
    form.reset({
      name: team?.name ?? "",
      description: team?.description ?? "",
    });
    setMemberIds([]);
  }, [open, team, form]);

  // Seed membership once the detail arrives (edit mode only).
  useEffect(() => {
    if (open && teamDetailQuery.data) {
      setMemberIds(teamDetailQuery.data.members.map((m) => m.id));
    }
  }, [open, teamDetailQuery.data]);

  const pending = createTeam.isPending || updateTeam.isPending;
  // Saving replaces the membership wholesale, so an edit must not submit
  // until the current members have actually loaded — otherwise the still
  // empty memberIds would silently wipe the team.
  const membersNotLoaded = isEdit && !teamDetailQuery.isSuccess;

  function toggleMember(id: string) {
    setMemberIds((ids) =>
      ids.includes(id) ? ids.filter((x) => x !== id) : [...ids, id],
    );
  }

  const selectedUsers = memberIds
    .map((id) => eligibleUsers.find((u) => u.id === id))
    .filter((u): u is User => u !== undefined);

  async function onSubmit(values: TeamValues) {
    if (membersNotLoaded) {
      return;
    }
    setError(null);
    try {
      if (isEdit) {
        await updateTeam.mutateAsync({
          id: team.id,
          body: { name: values.name, description: values.description },
          memberIds,
        });
      } else {
        await createTeam.mutateAsync({
          body: { name: values.name, description: values.description },
          memberIds,
        });
      }
      onOpenChange(false);
    } catch (err) {
      setError(err);
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{isEdit ? "Edit team" : "New team"}</DialogTitle>
          <DialogDescription>
            Teams are routing queues — tickets get assigned to them.
          </DialogDescription>
        </DialogHeader>

        <Form {...form}>
          <form
            onSubmit={form.handleSubmit(onSubmit)}
            className="space-y-4"
            noValidate
          >
            {error != null && <ProblemAlert error={error} />}
            {teamDetailQuery.isError && (
              <ProblemAlert error={teamDetailQuery.error} />
            )}

            <FormField
              control={form.control}
              name="name"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Name</FormLabel>
                  <FormControl>
                    <Input autoComplete="off" {...field} />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />

            <FormField
              control={form.control}
              name="description"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Description</FormLabel>
                  <FormControl>
                    <Input autoComplete="off" {...field} />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />

            <FormItem>
              <FormLabel>Members</FormLabel>
              <Popover open={pickerOpen} onOpenChange={setPickerOpen}>
                <PopoverTrigger asChild>
                  <Button
                    type="button"
                    variant="outline"
                    role="combobox"
                    aria-expanded={pickerOpen}
                    className="w-full justify-between font-normal"
                    disabled={isEdit && teamDetailQuery.isPending}
                  >
                    {memberIds.length === 0
                      ? "Select agents…"
                      : `${memberIds.length} member${memberIds.length === 1 ? "" : "s"}`}
                    <ChevronsUpDown
                      className="text-muted-foreground"
                      aria-hidden="true"
                    />
                  </Button>
                </PopoverTrigger>
                <PopoverContent
                  className="w-(--radix-popover-trigger-width) p-0"
                  align="start"
                >
                  <Command>
                    <CommandInput placeholder="Search agents…" />
                    <CommandList>
                      <CommandEmpty>No eligible agents found.</CommandEmpty>
                      <CommandGroup>
                        {eligibleUsers.map((user) => (
                          <CommandItem
                            key={user.id}
                            value={`${user.name} ${user.email}`}
                            onSelect={() => toggleMember(user.id)}
                          >
                            <Check
                              className={cn(
                                memberIds.includes(user.id)
                                  ? "opacity-100"
                                  : "opacity-0",
                              )}
                              aria-hidden="true"
                            />
                            <span className="truncate">
                              {user.name || user.email}
                            </span>
                            <span className="ml-auto truncate text-xs text-muted-foreground">
                              {user.role}
                            </span>
                          </CommandItem>
                        ))}
                      </CommandGroup>
                    </CommandList>
                  </Command>
                </PopoverContent>
              </Popover>
              <FormDescription>
                Only active agents and admins can join a queue.
              </FormDescription>
              {selectedUsers.length > 0 && (
                <div className="flex flex-wrap gap-1.5 pt-1">
                  {selectedUsers.map((user) => (
                    <Badge key={user.id} variant="secondary" className="gap-1">
                      {user.name || user.email}
                      <button
                        type="button"
                        aria-label={`Remove ${user.name || user.email}`}
                        onClick={() => toggleMember(user.id)}
                        className="rounded-full focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                      >
                        <X className="size-3" aria-hidden="true" />
                      </button>
                    </Badge>
                  ))}
                </div>
              )}
            </FormItem>

            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                onClick={() => onOpenChange(false)}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={pending || membersNotLoaded}>
                {pending
                  ? "Saving…"
                  : isEdit
                    ? "Save changes"
                    : "Create team"}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  );
}
