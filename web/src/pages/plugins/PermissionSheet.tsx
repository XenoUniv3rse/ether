import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  Input,
} from "@braedonsaunders/appkit-ui";
import {
  AlertTriangle,
  FolderOpen,
  Globe,
  Network,
  Plus,
  Shield,
  ShieldCheck,
  Users,
  X,
} from "lucide-react";
import { useState } from "react";
import { ChipGroup } from "../../components/fields";
import { useViewers, viewerName } from "../../components/pickers";
import {
  durablePermissionLine,
  listItemsLine,
  type DurableQuota,
  translatePermissionLine,
  type ListItemsGrant,
  type RuntimeLimits,
} from "./types";
import { getLocale, t } from "../../i18n";

export function ViewerGrantsPicker({
  value,
  onChange,
}: {
  value: number[];
  onChange: (next: number[]) => void;
}) {
  const { viewers } = useViewers();
  const options = (viewers ?? []).map((v) => ({
    value: v.id,
    label: viewerName(v),
  }));

  return (
    <div className="space-y-2 rounded-lg border border-border bg-bg-subtle p-3.5">
      <div className="flex items-center gap-2 text-xs font-medium text-fg">
        <Users size={14} className="text-fg-muted" aria-hidden />
        <span>{t("Viewers this plugin can access")}</span>
      </div>
      <p className="text-xs text-fg-muted">
        {t("Choose which household members share their activity with this plugin.")}
      </p>
      {options.length > 0 ? (
        <ChipGroup
          ariaLabel={t("Granted viewers")}
          options={options}
          value={value}
          onChange={onChange}
          size="sm"
        />
      ) : (
        <p className="text-xs text-fg-muted">{t("No viewers found on this server.")}</p>
      )}
    </div>
  );
}

export function FolderGrantsPicker({
  value,
  onChange,
}: {
  value: string[];
  onChange: (next: string[]) => void;
}) {
  const [draft, setDraft] = useState("");

  const addFolder = () => {
    const clean = draft.trim();
    if (!clean) return;
    if (!value.includes(clean)) {
      onChange([...value, clean]);
    }
    setDraft("");
  };

  return (
    <div className="space-y-2.5 rounded-lg border border-border bg-bg-subtle p-3.5">
      <div className="flex items-center gap-2 text-xs font-medium text-fg">
        <FolderOpen size={14} className="text-fg-muted" aria-hidden />
        <span>{t("Folders this plugin can read")}</span>
      </div>
      <p className="text-xs text-fg-muted">
        {t("This plugin can only list and read files inside the folders you add here.")}
      </p>
      {value.length > 0 ? (
        <ul className="space-y-1.5" aria-label={t("Granted folders")}>
          {value.map((folder) => (
            <li
              key={folder}
              className="flex items-center justify-between gap-2 rounded-md border border-border bg-surface px-2.5 py-1.5 text-xs"
            >
              <code className="min-w-0 flex-1 truncate font-mono text-fg">{folder}</code>
              <button
                type="button"
                aria-label={t("Remove {folder}", { folder })}
                onClick={() => onChange(value.filter((f) => f !== folder))}
                className="rounded p-0.5 text-fg-subtle hover:bg-surface-hover hover:text-fg"
              >
                <X size={13} />
              </button>
            </li>
          ))}
        </ul>
      ) : null}
      <div className="flex items-center gap-2">
        <Input
          value={draft}
          onChange={(e) => setDraft(e.currentTarget.value)}
          placeholder={t("/mnt/media/movies")}
          aria-label={t("Folder path")}
          className="flex-1 font-mono text-xs"
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              addFolder();
            }
          }}
        />
        <Button type="button" variant="outline" size="sm" onClick={addFolder}>
          <Plus size={14} /> {t("Add folder")}
        </Button>
      </div>
    </div>
  );
}

function cleanLocalHostInput(raw: string): string {
  const trimmed = raw.trim();
  const m = /^https?:\/\/([^/?#*]+)\/?$/i.exec(trimmed);
  return m ? m[1]! : trimmed;
}

function cleanOperatorHostInput(raw: string): string {
  const trimmed = raw.trim();
  if (!trimmed || trimmed === "*" || trimmed.toLowerCase() === "operator") return "";
  if (/^https?:\/\//i.test(trimmed)) {
    try {
      const u = new URL(trimmed);
      return u.host.toLowerCase();
    } catch {
      return "";
    }
  }
  const host = trimmed.split(/[/?#]/)[0]?.trim().toLowerCase() ?? "";
  if (!host || host === "operator" || host.includes("*")) {
    return "";
  }
  return host;
}

function operatorGrantCovers(approved: string[], candidate: string): boolean {
  const target = candidate.toLowerCase();
  const targetName = target.split(":")[0]!;
  return approved.some((entry) => {
    const clean = entry.toLowerCase();
    return clean === target || clean === targetName;
  });
}

export function OperatorHostGrantsPicker({
  value,
  pending = [],
  onChange,
}: {
  value: string[];
  pending?: string[];
  onChange: (next: string[]) => void;
}) {
  const [draft, setDraft] = useState("");

  const addHost = () => {
    const clean = cleanOperatorHostInput(draft);
    if (!clean) return;
    if (!value.some((h) => h.toLowerCase() === clean)) {
      onChange([...value, clean]);
    }
    setDraft("");
  };

  const approveHost = (host: string) => {
    const clean = cleanOperatorHostInput(host);
    if (!clean) return;
    if (!value.some((h) => h.toLowerCase() === clean)) {
      onChange([...value, clean]);
    }
  };

  const waiting = pending.filter((host) => !operatorGrantCovers(value, host));

  return (
    <div className="space-y-2.5 rounded-lg border border-border bg-bg-subtle p-3.5">
      <div className="flex items-center gap-2 text-xs font-medium text-fg">
        <Globe size={14} className="text-fg-muted" aria-hidden />
        <span>{t("Approved addresses")}</span>
      </div>
      <p className="text-xs text-fg-muted">
        {t("Lets this plugin connect to the server addresses you set and approve.")}
      </p>
      {waiting.length > 0 ? (
        <ul className="space-y-1.5" aria-label={t("Addresses waiting for approval")}>
          {waiting.map((host) => (
            <li
              key={host}
              className="flex items-center justify-between gap-2 rounded-md border border-warning/40 bg-warning/5 px-2.5 py-1.5 text-xs"
            >
              <span className="min-w-0 flex-1 truncate text-fg">
                {t("Needs approval to connect to {host}", { host })}
              </span>
              <Button type="button" variant="outline" size="sm" onClick={() => approveHost(host)}>
                {t("Approve")}
              </Button>
            </li>
          ))}
        </ul>
      ) : null}
      {value.length > 0 ? (
        <ul className="space-y-1.5" aria-label={t("Approved addresses")}>
          {value.map((host) => (
            <li
              key={host}
              className="flex items-center justify-between gap-2 rounded-md border border-border bg-surface px-2.5 py-1.5 text-xs"
            >
              <code className="min-w-0 flex-1 truncate font-mono text-fg">{host}</code>
              <button
                type="button"
                aria-label={t("Revoke {host}", { host })}
                onClick={() => onChange(value.filter((h) => h !== host))}
                className="rounded p-0.5 text-fg-subtle hover:bg-surface-hover hover:text-fg"
              >
                <X size={13} />
              </button>
            </li>
          ))}
        </ul>
      ) : null}
      <div className="flex items-center gap-2">
        <Input
          value={draft}
          onChange={(e) => setDraft(e.currentTarget.value)}
          placeholder={t("api.example.com")}
          aria-label={t("Server address")}
          className="flex-1 font-mono text-xs"
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              addHost();
            }
          }}
        />
        <Button type="button" variant="outline" size="sm" onClick={addHost}>
          <Plus size={14} /> {t("Approve address")}
        </Button>
      </div>
    </div>
  );
}

export function LocalHostGrantsPicker({
  value,
  blocked = [],
  onChange,
}: {
  value: string[];
  blocked?: string[];
  onChange: (next: string[]) => void;
}) {
  const [draft, setDraft] = useState("");

  const addHost = () => {
    const clean = cleanLocalHostInput(draft);
    if (!clean) return;
    if (!value.includes(clean)) {
      onChange([...value, clean]);
    }
    setDraft("");
  };

  const allowBlocked = (host: string) => {
    if (!value.includes(host)) {
      onChange([...value, host]);
    }
  };

  return (
    <div className="space-y-2.5 rounded-lg border border-border bg-bg-subtle p-3.5">
      <div className="flex items-center gap-2 text-xs font-medium text-fg">
        <Network size={14} className="text-fg-muted" aria-hidden />
        <span>{t("On your network")}</span>
      </div>
      <p className="text-xs text-fg-muted">
        {t(
          "Lets this plugin reach these addresses on your network, such as tuliprox or 192.168.1.20.",
        )}
      </p>
      {blocked.length > 0 ? (
        <ul className="space-y-1.5" aria-label={t("Blocked network addresses")}>
          {blocked.map((host) => (
            <li
              key={host}
              className="flex items-center justify-between gap-2 rounded-md border border-warning/40 bg-warning/5 px-2.5 py-1.5 text-xs"
            >
              <span className="min-w-0 flex-1 truncate text-fg">
                {t("Blocked {host} (on your network)", { host })}
              </span>
              <Button type="button" variant="outline" size="sm" onClick={() => allowBlocked(host)}>
                {t("Allow")}
              </Button>
            </li>
          ))}
        </ul>
      ) : null}
      {value.length > 0 ? (
        <ul className="space-y-1.5" aria-label={t("Allowed network addresses")}>
          {value.map((host) => (
            <li
              key={host}
              className="flex items-center justify-between gap-2 rounded-md border border-border bg-surface px-2.5 py-1.5 text-xs"
            >
              <code className="min-w-0 flex-1 truncate font-mono text-fg">{host}</code>
              <button
                type="button"
                aria-label={t("Remove {host}", { host })}
                onClick={() => onChange(value.filter((h) => h !== host))}
                className="rounded p-0.5 text-fg-subtle hover:bg-surface-hover hover:text-fg"
              >
                <X size={13} />
              </button>
            </li>
          ))}
        </ul>
      ) : null}
      <div className="flex items-center gap-2">
        <Input
          value={draft}
          onChange={(e) => setDraft(e.currentTarget.value)}
          placeholder={t("tuliprox:8901")}
          aria-label={t("Network address")}
          className="flex-1 font-mono text-xs"
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              addHost();
            }
          }}
        />
        <Button type="button" variant="outline" size="sm" onClick={addHost}>
          <Plus size={14} /> {t("Add")}
        </Button>
      </div>
    </div>
  );
}

export function PermissionSheet({
  permissions,
  verified,
  needsViewers,
  needsFolders,
  needsOperatorHosts,
  viewers,
  onChangeViewers,
  folders,
  onChangeFolders,
  operatorHosts,
  pendingOperatorHosts,
  onChangeOperatorHosts,
  addedPermissions,
  durableQuota,
  listItems,
  ownDraft = false,
}: {
  permissions: string[];
  verified: boolean;
  /** The owner's own Studio draft: no third-party trust warning. */
  ownDraft?: boolean;
  needsViewers?: boolean;
  needsFolders?: boolean;
  needsOperatorHosts?: boolean;
  viewers: number[];
  onChangeViewers: (next: number[]) => void;
  folders: string[];
  onChangeFolders: (next: string[]) => void;
  operatorHosts?: string[];
  pendingOperatorHosts?: string[];
  onChangeOperatorHosts?: (next: string[]) => void;
  addedPermissions?: string[];
  durableQuota?: DurableQuota;
  /** The list sync size asked for and allowed, when the plugin has lists. */
  listItems?: ListItemsGrant;
}) {
  const lines =
    permissions.length > 0 ? permissions : [t("Needs no access to your server or the network")];
  const listLine = listItems ? listItemsLine(listItems.requested) : "";
  const showLine = (line: string) => {
    if (line === "Stores saved data until deleted") return durablePermissionLine(durableQuota);
    const text = translatePermissionLine(line);
    // Your global limit allows less than the plugin asks: say so here.
    return listItems && text === listLine && listItems.granted < listItems.requested
      ? t("{line}; your limit allows {count}", {
          line: text,
          count: new Intl.NumberFormat(getLocale()).format(listItems.granted),
        })
      : text;
  };
  const noAccess =
    lines.length === 1 && lines[0] === "Needs no access to your server or the network";

  return (
    <div className="space-y-4" data-testid="permission-sheet">
      {!verified && !ownDraft ? (
        <Alert variant="warning">
          <AlertTitle className="flex items-center gap-2">
            <Badge variant="warning">
              <AlertTriangle size={12} aria-hidden /> {t("Unverified")}
            </Badge>
            <span>{t("Not reviewed by Ether")}</span>
          </AlertTitle>
          <AlertDescription>
            {t(
              "This plugin comes from an external address and has not been reviewed for the Ether catalogue. Install it only if you trust its author.",
            )}
          </AlertDescription>
        </Alert>
      ) : null}

      <div className="rounded-xl border border-border bg-surface p-4">
        <div className="mb-2.5 flex items-center justify-between gap-2">
          <h3 className="text-xs font-semibold uppercase tracking-wider text-fg-muted">
            {t("Permissions")}
          </h3>
          {verified ? (
            <Badge variant="success">
              <ShieldCheck size={12} aria-hidden /> {t("Verified")}
            </Badge>
          ) : null}
        </div>
        <ul className="space-y-2" aria-label={t("Requested permissions")}>
          {lines.map((line, i) => (
            <li key={`${i}-${line}`} className="flex items-start gap-2.5 text-sm text-fg">
              {noAccess ? (
                <ShieldCheck size={16} className="mt-0.5 shrink-0 text-success" aria-hidden />
              ) : (
                <Shield size={16} className="mt-0.5 shrink-0 text-fg-muted" aria-hidden />
              )}
              <span>{showLine(line)}</span>
            </li>
          ))}
        </ul>
      </div>

      {addedPermissions && addedPermissions.length > 0 ? (
        <Alert variant="warning">
          <AlertTitle>{t("New permissions requested")}</AlertTitle>
          <AlertDescription>
            <ul className="mt-1 list-disc space-y-1 pl-4 text-xs">
              {addedPermissions.map((line) => (
                <li key={line}>{showLine(line)}</li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      ) : null}

      {needsViewers ? <ViewerGrantsPicker value={viewers} onChange={onChangeViewers} /> : null}

      {needsFolders ? <FolderGrantsPicker value={folders} onChange={onChangeFolders} /> : null}

      {needsOperatorHosts && onChangeOperatorHosts ? (
        <OperatorHostGrantsPicker
          value={operatorHosts ?? []}
          pending={pendingOperatorHosts}
          onChange={onChangeOperatorHosts}
        />
      ) : null}
    </div>
  );
}

export function RuntimeLimitsCard({
  limits,
  requested,
  listItems,
}: {
  limits?: RuntimeLimits;
  requested?: Partial<RuntimeLimits>;
  listItems?: ListItemsGrant;
}) {
  if (!limits) return null;
  const resources: { label: string; value: (l?: Partial<RuntimeLimits>) => React.ReactNode }[] = [
    {
      label: t("Memory (MiB)"),
      value: (l) => (l?.memoryBytes ? l.memoryBytes / 1048576 : t("Default")),
    },
    { label: t("Time"), value: (l) => l?.deadline || t("Default") },
    {
      label: t("Work units"),
      value: (l) =>
        !l?.unitBudget ? t("Default") : l.unitBudget >= 2 ** 53 ? t("Unlimited") : l.unitBudget,
    },
    { label: t("Instances"), value: (l) => l?.poolSize || t("Default") },
    {
      label: t("Download (GiB)"),
      value: (l) => (l?.fetchBytes ? l.fetchBytes / 1073741824 : t("Default")),
    },
    {
      label: t("Archive member (GiB)"),
      value: (l) => (l?.archiveMemberBytes ? l.archiveMemberBytes / 1073741824 : t("Default")),
    },
    {
      label: t("Archive total (GiB)"),
      value: (l) => (l?.archiveExpandedBytes ? l.archiveExpandedBytes / 1073741824 : t("Default")),
    },
    { label: t("Archive members"), value: (l) => l?.archiveMembers || t("Default") },
    { label: t("Catalogue items"), value: (l) => l?.maxItems || t("Default") },
  ];
  return (
    <div className="rounded-xl border border-border bg-surface p-4">
      <h3 className="mb-2 text-sm font-medium">{t("Limits")}</h3>
      <table className="w-full text-xs">
        <thead>
          <tr>
            <th />
            <th>{t("Requested")}</th>
            <th>{t("Granted")}</th>
          </tr>
        </thead>
        <tbody>
          {resources.map((resource) => (
            <tr key={resource.label}>
              <th className="py-1 text-left">{resource.label}</th>
              <td className="text-center">{resource.value(requested)}</td>
              <td className="text-center">{resource.value(limits)}</td>
            </tr>
          ))}
          {listItems ? (
            <tr>
              <th className="py-1 text-left">{t("Titles per list")}</th>
              <td className="text-center">
                {new Intl.NumberFormat(getLocale()).format(listItems.requested)}
              </td>
              <td className="text-center">
                {new Intl.NumberFormat(getLocale()).format(listItems.granted)}
              </td>
            </tr>
          ) : null}
        </tbody>
      </table>
    </div>
  );
}
