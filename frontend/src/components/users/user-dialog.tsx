import { zodResolver } from "@hookform/resolvers/zod";
import { useEffect, useState } from "react";
import { useForm } from "react-hook-form";
import { z } from "zod";

import type { Role, User } from "@/api/types";
import { useCreateUser, useUpdateUser } from "@/api/users";
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

const ROLE_LABELS: Record<Role, string> = {
  customer: "Customer",
  agent: "Agent",
  admin: "Admin",
};

const userSchema = z.object({
  name: z.string().min(1, "Name is required"),
  email: z.email("Enter a valid email address"),
  role: z.enum(["customer", "agent", "admin"]),
  company: z.string(),
  // Optional in both modes: create allows password-less (OIDC-later) users,
  // edit keeps the current password when blank. The minimum matches the
  // server policy (internal/auth/validate.go MinPasswordLen).
  password: z
    .string()
    .refine((v) => v === "" || v.length >= 10, {
      message: "Must be at least 10 characters",
    }),
});

type UserValues = z.infer<typeof userSchema>;

/**
 * Create + edit share one dialog. Email is immutable after creation (M1 API
 * contract), so the field is disabled in edit mode.
 */
export function UserDialog({
  open,
  onOpenChange,
  user,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Present in edit mode; absent in create mode. */
  user?: User;
}) {
  const isEdit = user !== undefined;
  const createUser = useCreateUser();
  const updateUser = useUpdateUser();
  const [error, setError] = useState<unknown>(null);

  const form = useForm<UserValues>({
    resolver: zodResolver(userSchema),
    defaultValues: {
      name: "",
      email: "",
      role: "customer",
      company: "",
      password: "",
    },
  });

  // Re-seed the form each time the dialog opens for a (different) subject.
  useEffect(() => {
    if (!open) {
      return;
    }
    setError(null);
    form.reset({
      name: user?.name ?? "",
      email: user?.email ?? "",
      role: user?.role ?? "customer",
      company: user?.company ?? "",
      password: "",
    });
  }, [open, user, form]);

  const pending = createUser.isPending || updateUser.isPending;

  async function onSubmit(values: UserValues) {
    setError(null);
    try {
      if (isEdit) {
        await updateUser.mutateAsync({
          id: user.id,
          body: {
            name: values.name,
            role: values.role,
            company: values.company,
            ...(values.password !== "" && { password: values.password }),
          },
        });
      } else {
        await createUser.mutateAsync({
          email: values.email,
          name: values.name,
          role: values.role,
          company: values.company,
          ...(values.password !== "" && { password: values.password }),
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
          <DialogTitle>{isEdit ? "Edit user" : "New user"}</DialogTitle>
          <DialogDescription>
            {isEdit
              ? "Update profile, role, or password."
              : "Invite a teammate or register a customer."}
          </DialogDescription>
        </DialogHeader>

        <Form {...form}>
          <form
            onSubmit={form.handleSubmit(onSubmit)}
            className="space-y-4"
            noValidate
          >
            {error != null && <ProblemAlert error={error} />}

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
              name="email"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Email</FormLabel>
                  <FormControl>
                    <Input
                      type="email"
                      autoComplete="off"
                      disabled={isEdit}
                      {...field}
                    />
                  </FormControl>
                  {isEdit && (
                    <FormDescription>
                      Email cannot be changed in this release.
                    </FormDescription>
                  )}
                  <FormMessage />
                </FormItem>
              )}
            />

            <div className="grid grid-cols-2 gap-4">
              <FormField
                control={form.control}
                name="role"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Role</FormLabel>
                    <Select value={field.value} onValueChange={field.onChange}>
                      <FormControl>
                        <SelectTrigger className="w-full">
                          <SelectValue />
                        </SelectTrigger>
                      </FormControl>
                      <SelectContent>
                        {(Object.keys(ROLE_LABELS) as Role[]).map((role) => (
                          <SelectItem key={role} value={role}>
                            {ROLE_LABELS[role]}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    <FormMessage />
                  </FormItem>
                )}
              />

              <FormField
                control={form.control}
                name="company"
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>Company</FormLabel>
                    <FormControl>
                      <Input autoComplete="off" {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
            </div>

            <FormField
              control={form.control}
              name="password"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>
                    {isEdit ? "New password" : "Password"}
                  </FormLabel>
                  <FormControl>
                    <Input
                      type="password"
                      autoComplete="new-password"
                      {...field}
                    />
                  </FormControl>
                  <FormDescription>
                    {isEdit
                      ? "Leave blank to keep the current password. Changing it signs the user out everywhere."
                      : "Optional — leave blank for users who will sign in via SSO later."}
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />

            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                onClick={() => onOpenChange(false)}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={pending}>
                {pending
                  ? "Saving…"
                  : isEdit
                    ? "Save changes"
                    : "Create user"}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  );
}
