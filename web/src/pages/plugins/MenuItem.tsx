import { cn } from "@braedonsaunders/appkit-ui";
import type { ReactNode } from "react";

/** One row of a plugin actions menu (the drawer's ⋯ and Studio's Actions). */
export function MenuItem({
  icon,
  label,
  hint,
  onClick,
  disabled,
  danger,
}: {
  icon: ReactNode;
  label: string;
  hint?: string;
  onClick: () => void;
  disabled?: boolean;
  danger?: boolean;
}) {
  return (
    <button
      type="button"
      role="menuitem"
      disabled={disabled}
      onClick={onClick}
      className={cn(
        "flex w-full items-start gap-2.5 rounded-md px-2.5 py-2 text-left text-sm transition-colors disabled:cursor-not-allowed disabled:opacity-50",
        danger ? "text-danger hover:bg-danger/10" : "text-fg hover:bg-surface-hover",
      )}
    >
      <span className="mt-0.5 shrink-0">{icon}</span>
      <span>
        {label}
        {hint ? <span className="block text-xs text-fg-subtle">{hint}</span> : null}
      </span>
    </button>
  );
}
