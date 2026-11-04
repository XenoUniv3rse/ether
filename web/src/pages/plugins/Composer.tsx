import { Button, Textarea, cn } from "@braedonsaunders/appkit-ui";
import { ArrowUp, Square } from "lucide-react";
import { useEffect, useRef } from "react";
import { t } from "../../i18n";

/** Studio's prompt box: one line that grows, send or stop. */
export function Composer({
  value,
  onChange,
  onSend,
  working,
  onStop,
  placeholder,
  compact,
}: {
  value: string;
  onChange: (v: string) => void;
  onSend: (text: string) => void;
  working?: boolean;
  onStop?: () => void;
  placeholder: string;
  compact?: boolean;
}) {
  const ref = useRef<HTMLTextAreaElement>(null);
  useEffect(() => {
    if (!compact) ref.current?.focus();
  }, [compact, value]);
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        onSend(value);
      }}
      className={cn(
        "shrink-0 border-t border-border bg-bg-subtle/95 backdrop-blur",
        compact ? "p-3" : "px-4 py-4",
      )}
    >
      <div
        className={cn(
          "mx-auto flex w-full items-end gap-2 rounded-2xl border border-border bg-surface p-2 shadow-sm transition focus-within:border-primary/50 focus-within:shadow-md",
          !compact && "max-w-3xl",
        )}
      >
        <Textarea
          ref={ref}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              onSend(value);
            }
          }}
          rows={1}
          aria-label={placeholder}
          placeholder={placeholder}
          className="min-h-10 w-full min-w-0 flex-1 resize-none border-0 bg-transparent text-base shadow-none focus-visible:ring-0"
        />
        {working ? (
          <Button
            type="button"
            size="icon"
            variant="outline"
            onClick={onStop}
            aria-label={t("Stop")}
            className="shrink-0 rounded-full"
          >
            <Square size={14} />
          </Button>
        ) : (
          <Button
            type="submit"
            size="icon"
            disabled={!value.trim()}
            aria-label={t("Build")}
            className="shrink-0 rounded-full"
          >
            <ArrowUp size={16} />
          </Button>
        )}
      </div>
    </form>
  );
}
