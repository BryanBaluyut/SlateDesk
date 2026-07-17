import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import { Ellipsis, Plus, UserRound } from "lucide-react";
import { useState } from "react";

import type { Role, User } from "@/api/types";
import { useDeactivateUser, useUpdateUser, usersQueryOptions } from "@/api/users";
import { PageContainer, PageHeader, PagePlaceholder } from "@/components/layout/page";
import { initials } from "@/components/layout/topbar";
import { ProblemAlert } from "@/components/problem-alert";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { UserDialog } from "@/components/users/user-dialog";
import { cn } from "@/lib/utils";

export const Route = createFileRoute("/_auth/users")({
  component: UsersPage,
});

const ROLE_BADGE: Record<Role, "default" | "secondary" | "outline"> = {
  admin: "default",
  agent: "secondary",
  customer: "outline",
};

function UsersPage() {
  const usersQuery = useQuery(usersQueryOptions);
  const deactivateUser = useDeactivateUser();
  const updateUser = useUpdateUser();

  const [createOpen, setCreateOpen] = useState(false);
  const [editing, setEditing] = useState<User | null>(null);
  const [deactivating, setDeactivating] = useState<User | null>(null);

  const users = usersQuery.data ?? [];

  return (
    <PageContainer>
      <PageHeader
        title="Users"
        description={
          usersQuery.isSuccess
            ? `${users.length} ${users.length === 1 ? "person" : "people"} on this instance.`
            : "People on this instance."
        }
      >
        <Button size="sm" onClick={() => setCreateOpen(true)}>
          <Plus aria-hidden="true" />
          New user
        </Button>
      </PageHeader>

      {usersQuery.isError && <ProblemAlert error={usersQuery.error} />}

      {usersQuery.isPending && (
        <div className="space-y-2">
          {Array.from({ length: 4 }, (_, i) => (
            <Skeleton key={i} className="h-11 w-full" />
          ))}
        </div>
      )}

      {usersQuery.isSuccess && users.length === 0 && (
        <PagePlaceholder
          icon={UserRound}
          title="No users yet"
          description="Create the first agent or customer to get started."
        />
      )}

      {usersQuery.isSuccess && users.length > 0 && (
        <div className="overflow-x-auto rounded-lg border">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Name</TableHead>
                <TableHead>Email</TableHead>
                <TableHead>Role</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="w-12" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {users.map((user) => (
                <TableRow
                  key={user.id}
                  className={cn(!user.active && "opacity-60")}
                >
                  <TableCell>
                    <div className="flex items-center gap-2.5">
                      <Avatar className="size-6">
                        <AvatarFallback className="text-[10px]">
                          {initials(user.name, user.email)}
                        </AvatarFallback>
                      </Avatar>
                      <div className="min-w-0">
                        <div className="truncate font-medium">
                          {user.name || "—"}
                        </div>
                        {user.company && (
                          <div className="truncate text-xs text-muted-foreground">
                            {user.company}
                          </div>
                        )}
                      </div>
                    </div>
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {user.email}
                  </TableCell>
                  <TableCell>
                    <Badge variant={ROLE_BADGE[user.role]}>{user.role}</Badge>
                  </TableCell>
                  <TableCell>
                    {user.active ? (
                      <span className="flex items-center gap-1.5 text-sm">
                        <span
                          className="size-1.5 rounded-full bg-emerald-500"
                          aria-hidden="true"
                        />
                        Active
                      </span>
                    ) : (
                      <span className="text-sm text-muted-foreground">
                        Deactivated
                      </span>
                    )}
                  </TableCell>
                  <TableCell>
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button
                          variant="ghost"
                          size="icon-xs"
                          aria-label={`Actions for ${user.name || user.email}`}
                        >
                          <Ellipsis aria-hidden="true" />
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem onSelect={() => setEditing(user)}>
                          Edit
                        </DropdownMenuItem>
                        <DropdownMenuSeparator />
                        {user.active ? (
                          <DropdownMenuItem
                            variant="destructive"
                            onSelect={() => setDeactivating(user)}
                          >
                            Deactivate
                          </DropdownMenuItem>
                        ) : (
                          <DropdownMenuItem
                            onSelect={() =>
                              updateUser.mutate({
                                id: user.id,
                                body: { active: true },
                              })
                            }
                          >
                            Reactivate
                          </DropdownMenuItem>
                        )}
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      <UserDialog open={createOpen} onOpenChange={setCreateOpen} />
      <UserDialog
        open={editing !== null}
        onOpenChange={(open) => {
          if (!open) setEditing(null);
        }}
        user={editing ?? undefined}
      />

      <AlertDialog
        open={deactivating !== null}
        onOpenChange={(open) => {
          if (!open) setDeactivating(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              Deactivate {deactivating?.name || deactivating?.email}?
            </AlertDialogTitle>
            <AlertDialogDescription>
              They will be signed out everywhere and can no longer log in.
              Their history is kept and the account can be reactivated later.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (deactivating) {
                  deactivateUser.mutate(deactivating.id);
                }
              }}
            >
              Deactivate
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </PageContainer>
  );
}
