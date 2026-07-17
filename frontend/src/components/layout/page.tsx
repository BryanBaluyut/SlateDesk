import type { LucideIcon } from "lucide-react";

/** Consistent page chrome: calm header row with optional actions. */
export function PageHeader({
  title,
  description,
  children,
}: {
  title: string;
  description?: string;
  children?: React.ReactNode;
}) {
  return (
    <div className="flex items-start justify-between gap-4 pb-5">
      <div>
        <h1 className="text-lg font-semibold tracking-tight">{title}</h1>
        {description && (
          <p className="mt-0.5 text-sm text-muted-foreground">{description}</p>
        )}
      </div>
      {children && <div className="flex shrink-0 items-center gap-2">{children}</div>}
    </div>
  );
}

export function PageContainer({ children }: { children: React.ReactNode }) {
  return <div className="mx-auto w-full max-w-5xl px-6 py-6">{children}</div>;
}

/** Empty state for placeholder destinations (Tickets, Settings in M1). */
export function PagePlaceholder({
  icon: Icon,
  title,
  description,
}: {
  icon: LucideIcon;
  title: string;
  description: string;
}) {
  return (
    <div className="flex h-full min-h-64 flex-col items-center justify-center gap-3 rounded-lg border border-dashed p-12 text-center">
      <div className="flex size-10 items-center justify-center rounded-full border bg-muted/50">
        <Icon className="size-5 text-muted-foreground" aria-hidden="true" />
      </div>
      <div>
        <h2 className="text-sm font-medium">{title}</h2>
        <p className="mt-1 max-w-sm text-sm text-muted-foreground">
          {description}
        </p>
      </div>
    </div>
  );
}
