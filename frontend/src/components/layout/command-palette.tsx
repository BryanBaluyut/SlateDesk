import { useNavigate } from "@tanstack/react-router";
import { Moon, Sun } from "lucide-react";

import { NAV_ITEMS } from "@/components/layout/nav";
import { useTheme } from "@/components/theme";
import {
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  CommandSeparator,
} from "@/components/ui/command";

/**
 * cmdk palette stub (M1): navigation plus a theme action. Tickets search
 * and richer commands arrive with the ticket core in M2.
 */
export function CommandPalette({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const navigate = useNavigate();
  const { theme, toggleTheme } = useTheme();

  function run(action: () => void) {
    onOpenChange(false);
    action();
  }

  return (
    <CommandDialog
      open={open}
      onOpenChange={onOpenChange}
      title="Command palette"
      description="Type a command or search"
    >
      <CommandInput placeholder="Type a command or search…" />
      <CommandList>
        <CommandEmpty>No results found.</CommandEmpty>
        <CommandGroup heading="Go to">
          {NAV_ITEMS.map((item) => (
            <CommandItem
              key={item.to}
              onSelect={() => run(() => void navigate({ to: item.to }))}
            >
              <item.icon aria-hidden="true" />
              {item.label}
              {item.soon && (
                <span className="ml-auto text-xs text-muted-foreground">
                  soon
                </span>
              )}
            </CommandItem>
          ))}
        </CommandGroup>
        <CommandSeparator />
        <CommandGroup heading="Preferences">
          <CommandItem onSelect={() => run(toggleTheme)}>
            {theme === "dark" ? (
              <Sun aria-hidden="true" />
            ) : (
              <Moon aria-hidden="true" />
            )}
            Switch to {theme === "dark" ? "light" : "dark"} mode
          </CommandItem>
        </CommandGroup>
      </CommandList>
    </CommandDialog>
  );
}
