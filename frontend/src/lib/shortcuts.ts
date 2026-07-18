import { useEffect } from "react";

/**
 * Tiny in-app shortcut bus. One global keydown listener (mounted in the
 * authed layout) translates keys into named actions; interested components
 * subscribe with useShortcut. Decouples the keymap from the component tree —
 * the queue pane, thread composer, and context sidebar live in different
 * routes but share one keyboard map.
 */

export type ShortcutAction =
  | "queue-down" // j
  | "queue-up" // k
  | "queue-open" // Enter / o
  | "reply" // r — focus composer in reply mode
  | "note" // n — focus composer in internal-note mode
  | "assign" // a — open assignee combobox
  | "status" // s — open status select
  | "priority" // p — open priority select
  | "sidebar" // x — toggle context sidebar
  | "search" // / — focus queue search
  | "help"; // ? — shortcuts help dialog

type Handler = () => void;

const handlers = new Map<ShortcutAction, Set<Handler>>();

export function emitShortcut(action: ShortcutAction): boolean {
  const set = handlers.get(action);
  if (!set || set.size === 0) return false;
  for (const handler of set) handler();
  return true;
}

/** Subscribe to a shortcut action for the lifetime of the component. */
export function useShortcut(action: ShortcutAction, handler: Handler) {
  useEffect(() => {
    let set = handlers.get(action);
    if (!set) {
      set = new Set();
      handlers.set(action, set);
    }
    set.add(handler);
    return () => {
      set.delete(handler);
    };
  }, [action, handler]);
}

const KEYMAP: Record<string, ShortcutAction> = {
  j: "queue-down",
  k: "queue-up",
  o: "queue-open",
  Enter: "queue-open",
  r: "reply",
  n: "note",
  a: "assign",
  s: "status",
  p: "priority",
  x: "sidebar",
  "/": "search",
  "?": "help",
};

/**
 * True when the event originates somewhere plain letter keys must not be
 * hijacked: text fields, contenteditable, or any open overlay (dialogs,
 * dropdowns, selects — Radix portals) which have their own keyboard handling.
 */
function isShortcutSafeTarget(e: KeyboardEvent): boolean {
  const el = e.target;
  if (!(el instanceof Element)) return true;
  if (el instanceof HTMLElement && el.isContentEditable) return false;
  const tag = el.tagName;
  if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return false;
  if (
    el.closest(
      '[role="dialog"], [role="listbox"], [role="menu"], [data-radix-popper-content-wrapper]',
    )
  ) {
    return false;
  }
  return true;
}

/**
 * The one global keydown listener. Mount exactly once (authed layout).
 * Handled keys are preventDefault-ed so e.g. "/" never types into the page.
 */
export function useShortcutListener() {
  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      const action = KEYMAP[e.key];
      if (!action) return;
      if (!isShortcutSafeTarget(e)) return;
      // Enter is also how buttons and links are activated from the
      // keyboard; never steal it from a focused interactive control, or
      // Tab+Enter users can neither follow links nor press buttons.
      // (Plain letters are safe — controls do not consume them.)
      if (
        e.key === "Enter" &&
        e.target instanceof Element &&
        e.target.closest(
          'button, a[href], summary, [role="button"], [role="link"]',
        )
      ) {
        return;
      }
      if (emitShortcut(action)) {
        e.preventDefault();
      }
    }
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, []);
}
