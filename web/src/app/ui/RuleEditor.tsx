// SPDX-License-Identifier: AGPL-3.0-only

// The T0 rules editor (spec §8, #155): the interface for an engine that has been
// filtering mail since 1.0 with no client at all.
//
// Two things drive the shape of this file.
//
// THE CONDITION TREE IS RECURSIVE, so the editor is too. A rule's conditions are
// a nestable all/any tree, and flattening it to a list of ANDed rows — the usual
// shortcut — would make the "any of these senders, AND a subject match"
// rule unexpressible in the UI while remaining expressible on the wire. So
// ConditionNode renders itself.
//
// NOTHING HERE OFFERS WHAT THE SERVER REFUSES. moveToMailbox and junk are
// defined in the engine but rejected at save (they need IMAP-writeback work T0
// does not do), so they are absent from the action list rather than present and
// failing. Same for the condition fields: the list is exactly what
// internal/rules validates.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { JSX } from "react";

import type {
  Label,
  Rule,
  RuleAction,
  RuleActionType,
  RuleApplyResult,
  RuleAreaFiling,
  RuleCondition,
  RuleField,
  RuleOp,
  RulePreview,
  RuleTightenResult,
} from "../../core/jmap/types";
import { findDuplicateRule, nameForDraft } from "../../core/ruleIdentity";
import type { DuplicateRule } from "../../core/ruleIdentity";
import { stripBidiControls } from "../../core/hostileText";
import { useApp, useMailState } from "../state";
import { t } from "../i18n";
import type { MessageKey } from "../i18n";

/** The condition fields the server validates, in the order a person reaches for
 * them. `header` is last because it is the escape hatch, not the common case. */
const FIELDS: RuleField[] = [
  "from",
  "fromDomain",
  "subject",
  "to",
  "alias",
  "listId",
  "listUnsubscribe",
  "header",
];

/** Operators offered per field. listUnsubscribe is a presence test — "contains"
 * on an unsubscribe URL is not a question anyone means to ask. */
/** One row of a rule-preview sample list: who a matching message is from, and its
 * subject.
 *
 * BOTH ARE MESSAGE-DERIVED TEXT IN OUR OWN DOM, so both are bidi-stripped
 * (Forgejo #307, core/hostileText.ts). This surface is easy to overlook — it is a
 * settings panel, not the mail list — but a rule preview is precisely where a
 * user is deciding whether a rule matches the RIGHT mail, and a `from` that has
 * silently reordered the `subject` beside it (they share one line box, see
 * `.rule-preview-samples` in styles.css) is a bad place to be reading carefully.
 *
 * A component rather than three copies of the same two spans: the reason has to
 * live somewhere, and the fourth copy is the one that would forget. */
function PreviewSample({ from, subject }: { from: string; subject: string }): JSX.Element {
  return (
    <>
      <span className="from">{stripBidiControls(from)}</span>
      <span className="subject">{stripBidiControls(subject)}</span>
    </>
  );
}

function opsFor(field: RuleField): RuleOp[] {
  if (field === "listUnsubscribe") return ["exists"];
  return ["equals", "contains", "matches", "exists"];
}

const ACTIONS: RuleActionType[] = [
  "addLabel",
  "markPriority",
  "flag",
  "skipInbox",
  "stop",
];

const fieldLabel = (f: RuleField): string => t(`rules.field.${f}` as MessageKey);
const opLabel = (o: RuleOp): string => t(`rules.op.${o}` as MessageKey);
const actionLabel = (a: RuleActionType): string => t(`rules.action.${a}` as MessageKey);

/** A new leaf condition. */
const newLeaf = (): RuleCondition => ({ field: "from", op: "contains", value: "" });

/** True when the node is a boolean group rather than a leaf. */
const isGroup = (c: RuleCondition): boolean =>
  (c.all?.length ?? 0) > 0 || (c.any?.length ?? 0) > 0 || c.all !== undefined || c.any !== undefined;

// ---- the recursive condition editor ---------------------------------------

interface NodeProps {
  node: RuleCondition;
  onChange: (next: RuleCondition) => void;
  onRemove: (() => void) | null;
  depth: number;
}

function ConditionNode({ node, onChange, onRemove, depth }: NodeProps): JSX.Element {
  if (isGroup(node)) {
    const kind: "all" | "any" = node.any !== undefined ? "any" : "all";
    const children = (kind === "any" ? node.any : node.all) ?? [];
    const setChildren = (next: RuleCondition[]): void =>
      onChange(kind === "any" ? { any: next } : { all: next });

    return (
      <div className={`rule-group rule-depth-${Math.min(depth, 3)}`}>
        <div className="rule-group-header">
          <select
            aria-label={t("rules.conditions")}
            value={kind}
            onChange={(e) => {
              const next = e.target.value as "all" | "any";
              onChange(next === "any" ? { any: children } : { all: children });
            }}
          >
            <option value="all">{t("rules.matchAll")}</option>
            <option value="any">{t("rules.matchAny")}</option>
          </select>
          {onRemove ? (
            <button type="button" className="link-button" onClick={onRemove}>
              {t("rules.remove")}
            </button>
          ) : null}
        </div>

        <div className="rule-group-body">
          {children.map((child, i) => (
            <ConditionNode
              key={i}
              node={child}
              depth={depth + 1}
              onChange={(next) => setChildren(children.map((c, j) => (j === i ? next : c)))}
              onRemove={() => setChildren(children.filter((_, j) => j !== i))}
            />
          ))}
          <div className="rule-group-actions">
            <button
              type="button"
              className="link-button"
              onClick={() => setChildren([...children, newLeaf()])}
            >
              + {t("rules.addCondition")}
            </button>
            {/* Nesting is capped in the UI at a depth a person can still read.
                The engine has no limit, but a four-deep boolean tree in a form is
                not something anyone can verify by looking at it. */}
            {depth < 2 ? (
              <button
                type="button"
                className="link-button"
                onClick={() => setChildren([...children, { all: [newLeaf()] }])}
              >
                + {t("rules.addGroup")}
              </button>
            ) : null}
          </div>
        </div>
      </div>
    );
  }

  const field = node.field ?? "from";
  const ops = opsFor(field);
  const wanted = node.op ?? "contains";
  // opsFor never returns an empty list, but the fallback is written so the type
  // says so too rather than asserting it.
  const op: RuleOp = ops.includes(wanted) ? wanted : (ops[0] ?? "contains");

  return (
    <div className="rule-leaf">
      <select
        aria-label={t("rules.conditions")}
        value={field}
        onChange={(e) => {
          const nextField = e.target.value as RuleField;
          const nextOps = opsFor(nextField);
          onChange({
            ...node,
            field: nextField,
            // Keep the operator when the new field still supports it; otherwise
            // fall back rather than leaving an op the server will reject.
            op: nextOps.includes(op) ? op : nextOps[0],
            ...(nextField === "header" ? {} : { header: undefined }),
          });
        }}
      >
        {FIELDS.map((f) => (
          <option key={f} value={f}>
            {fieldLabel(f)}
          </option>
        ))}
      </select>

      {field === "header" ? (
        <input
          type="text"
          aria-label={t("rules.headerName")}
          placeholder={t("rules.headerPlaceholder")}
          value={node.header ?? ""}
          onChange={(e) => onChange({ ...node, header: e.target.value })}
        />
      ) : null}

      <select
        aria-label={t("rules.op.equals")}
        value={op}
        onChange={(e) => onChange({ ...node, op: e.target.value as RuleOp })}
      >
        {ops.map((o) => (
          <option key={o} value={o}>
            {opLabel(o)}
          </option>
        ))}
      </select>

      {op === "exists" ? null : (
        <input
          type="text"
          aria-label={t("rules.value")}
          value={node.value ?? ""}
          onChange={(e) => onChange({ ...node, value: e.target.value })}
        />
      )}

      {onRemove ? (
        <button
          type="button"
          className="icon-button"
          aria-label={t("rules.remove")}
          onClick={onRemove}
        >
          ✕
        </button>
      ) : null}
    </div>
  );
}

// ---- the action editor -----------------------------------------------------

/** The sentinel option that switches the picker into "name a new label" mode. It
 * is not a value a rule can ever hold — selecting it swaps the control, and the
 * action's label stays whatever it was until a real label id replaces it.
 *
 * WRITTEN AS AN ESCAPE, NOT A LITERAL NUL. It used to be an actual U+0000 byte
 * in this source file, which works at runtime and quietly made git treat the
 * whole file as BINARY: no diffs, no line-level review, no real merges, on the
 * largest file in the client. The escape is byte-identical at runtime and leaves
 * the source readable. NUL is still the right sentinel — no label id, keyword
 * atom or name can contain one — it just does not belong in the file verbatim. */
const CREATE_NEW = `${String.fromCharCode(0)}create`;

/**
 * Choose the label an addLabel action applies — or make one without leaving
 * (#166).
 *
 * Setting up categories is exactly when none of the labels exist yet, so a
 * picker over existing labels dead-ends the first rule someone ever writes.
 *
 * THE ACTION IS ONLY EVER SET TO A CREATED LABEL'S ID, never to the typed text.
 * store.resolveRuleLabel accepts a name, but applyRuleAction SKIPS SILENTLY when
 * it does not resolve (deliberately — a broken rule must never fail an ingest),
 * so a rule naming a label that does not exist saves cleanly, reads as enabled,
 * and does nothing forever. Writing the name in would manufacture exactly that.
 */
function LabelPicker({
  action,
  labels,
  onChange,
}: {
  action: RuleAction;
  labels: Label[];
  onChange: (a: RuleAction) => void;
}): JSX.Element {
  const { actions, pushToast } = useApp();
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const nameRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (creating) nameRef.current?.focus();
  }, [creating]);

  const create = async (): Promise<void> => {
    const trimmed = name.trim();
    if (!trimmed || busy) return;
    setBusy(true);
    const created = await actions.createLabel(trimmed);
    setBusy(false);
    // createLabel already surfaced the reason; leave the field alone so the name
    // is not lost and the attempt can be repeated.
    if (!created) return;
    if (!created.keyword && !created.id) {
      pushToast({ kind: "error", message: t("rules.labelCreateFailed") });
      return;
    }
    onChange({ ...action, label: created.id });
    setCreating(false);
    setName("");
  };

  if (creating) {
    return (
      <span className="rule-label-create">
        <input
          ref={nameRef}
          type="text"
          aria-label={t("rules.newLabel")}
          placeholder={t("rules.newLabelPlaceholder")}
          value={name}
          disabled={busy}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => {
            // Enter must not submit the surrounding form: this creates a label,
            // it does not save the rule.
            if (e.key === "Enter") {
              e.preventDefault();
              void create();
            }
            if (e.key === "Escape") {
              e.preventDefault();
              setCreating(false);
              setName("");
            }
          }}
        />
        <button type="button" className="link-button" disabled={busy || !name.trim()} onClick={() => void create()}>
          {busy ? t("rules.creating") : t("labels.create")}
        </button>
        <button
          type="button"
          className="link-button"
          onClick={() => {
            setCreating(false);
            setName("");
          }}
        >
          {t("rules.cancel")}
        </button>
      </span>
    );
  }

  return (
    <select
      aria-label={t("rules.action.addLabel")}
      value={action.label ?? ""}
      onChange={(e) => {
        if (e.target.value === CREATE_NEW) {
          setCreating(true);
          return;
        }
        onChange({ ...action, label: e.target.value });
      }}
    >
      <option value="">—</option>
      {labels.map((l) => (
        <option key={l.id} value={l.id}>
          {l.name}
        </option>
      ))}
      <option value={CREATE_NEW}>{t("rules.newLabel")}</option>
    </select>
  );
}

function ActionRow({
  action,
  labels,
  onChange,
  onRemove,
}: {
  action: RuleAction;
  labels: Label[];
  onChange: (a: RuleAction) => void;
  onRemove: () => void;
}): JSX.Element {
  return (
    <div className="rule-action">
      <select
        aria-label={t("rules.actions")}
        value={action.type}
        onChange={(e) => {
          const type = e.target.value as RuleActionType;
          onChange({ type, ...(type === "addLabel" ? { label: action.label } : {}) });
        }}
      >
        {ACTIONS.map((a) => (
          <option key={a} value={a}>
            {actionLabel(a)}
          </option>
        ))}
      </select>

      {action.type === "addLabel" ? <LabelPicker action={action} labels={labels} onChange={onChange} /> : null}

      <button type="button" className="icon-button" aria-label={t("rules.remove")} onClick={onRemove}>
        ✕
      </button>
    </div>
  );
}

// ---- the form --------------------------------------------------------------

/** A rule being edited: an existing one, or a draft with no id yet. */
export interface RuleDraft {
  id?: string;
  name: string;
  priority: number;
  conditions: RuleCondition;
  actions: RuleAction[];
  enabled: boolean;
  /**
   * The name is still OURS to choose — the user has not typed one (#292).
   *
   * Tracked as an explicit flag rather than inferred by comparing the field to
   * the last default we generated, which is the obvious implementation and is
   * wrong: it breaks the moment someone types the default by hand, and then
   * their deliberate name silently changes under them when they pick a label.
   * "Has the user touched this field" is a fact about an event, so it is
   * recorded when the event happens.
   */
  nameAuto?: boolean;
  /**
   * The prefill's own suggested name (RuleSuggestion.defaultName), kept as the
   * legible half of an auto-generated name — the sender's display name rather
   * than its address. See ruleNameKey.
   */
  nameBase?: string;
}

export function RuleForm({
  draft,
  existing,
  onEditExisting,
  onDone,
  onCancel,
}: {
  draft: RuleDraft;
  /** Every rule the user has, for duplicate detection (#292) — or null when the
   * list has not loaded or failed to. Null SKIPS the check rather than blocking
   * the save: the check is advisory, and the server's UNIQUE constraint plus
   * createRule's retry are what actually keep two rules from colliding. */
  existing?: readonly Rule[] | null;
  /** Open the duplicate the user chose to edit instead. */
  onEditExisting?: (rule: Rule) => void;
  onDone: () => void;
  onCancel: () => void;
}): JSX.Element {
  const { actions, pushToast } = useApp();
  const state = useMailState();
  const [form, setForm] = useState<RuleDraft>(draft);
  const [nameAuto, setNameAuto] = useState<boolean>(draft.nameAuto ?? false);
  // The duplicate we are asking about, and whether the user has already said
  // "create it anyway" — asking twice about the same rule would be a dialog
  // that cannot be dismissed.
  const [duplicate, setDuplicate] = useState<DuplicateRule | null>(null);
  const [duplicateAcked, setDuplicateAcked] = useState(false);
  const [preview, setPreview] = useState<RulePreview | null>(null);
  const [previewing, setPreviewing] = useState(false);
  // The preview describes the conditions as they were when it ran. Editing them
  // afterwards makes it stale, and a stale blast radius is exactly the kind of
  // confident wrong number this feature exists to avoid — so it is marked, not
  // silently kept.
  const [previewStale, setPreviewStale] = useState(false);
  const [saving, setSaving] = useState(false);

  const labels = useMemo(
    () => Object.values(state.labels).sort((a, b) => a.name.localeCompare(b.name)),
    [state.labels],
  );

  const setConditions = (conditions: RuleCondition): void => {
    setForm((f) => ({ ...f, conditions }));
    setPreviewStale(true);
    // The conditions moved, so any duplicate we were asking about is an answer
    // to a question that no longer exists.
    setDuplicate(null);
    setDuplicateAcked(false);
  };

  /** The display name of the label this rule applies, or "" — resolved the way
   * the server resolves an addLabel target, so a rule naming its label by
   * keyword or name gets named after the label a person would recognise. */
  const targetLabelName = (() => {
    const target = form.actions.find((a) => a.type === "addLabel" && (a.label ?? "").trim())?.label;
    return target ? (resolveTarget(target, state.labels)?.name ?? "") : "";
  })();

  // THE LABEL IS CHOSEN HERE, NOT AT PREFILL (#292), which is the whole reason
  // the name cannot be finalised in suggestionsFor: at the moment the draft is
  // built there is no label to name it after. So the default name is recomputed
  // as the label and the conditions change — and it is written into the visible
  // field rather than substituted at save time, because a name the user never
  // saw is a name they cannot disagree with.
  //
  // Guarded on nameAuto, which the name input clears permanently. Once someone
  // has typed, nothing here writes to the field again.
  useEffect(() => {
    setForm((f) => {
      const next = nameForDraft(
        { ...f, nameAuto },
        targetLabelName,
        (key, label) => t("rules.autoName", { key, label }),
      );
      return f.name === next ? f : { ...f, name: next };
    });
  }, [nameAuto, targetLabelName, form.conditions]);

  const runPreview = useCallback(async (): Promise<void> => {
    setPreviewing(true);
    // Tell the server which label this rule applies, so the blast radius can
    // report CONFLICTS — matched mail already filed elsewhere by hand. Without
    // it the preview still counts, it just cannot warn.
    const target = form.actions.find((a) => a.type === "addLabel" && a.label)?.label;
    const res = await actions.previewRule(form.conditions, target ? { targetLabelId: target } : {});
    setPreviewing(false);
    if (res) {
      setPreview(res);
      setPreviewStale(false);
    }
  }, [actions, form.conditions]);

  /** The write itself, once every question has been answered. */
  const commit = async (): Promise<void> => {
    setSaving(true);
    const body = {
      name: form.name.trim(),
      priority: form.priority,
      conditions: form.conditions,
      actions: form.actions,
      enabled: form.enabled,
    };
    const ok = form.id
      ? await actions.updateRule(form.id, body)
      : (await actions.createRule(body)) !== null;
    setSaving(false);
    if (ok) {
      pushToast({ kind: "info", message: form.id ? t("rules.saved") : t("rules.created") });
      onDone();
    }
  };

  const save = async (): Promise<void> => {
    if (!form.name.trim()) {
      pushToast({ kind: "error", message: t("rules.needsName") });
      return;
    }
    if (form.actions.length === 0) {
      pushToast({ kind: "error", message: t("rules.needsAction") });
      return;
    }
    // An addLabel with no label is an INERT rule, not a partial one (#166). The
    // server skips an unresolvable label rather than failing the ingest, so this
    // would save cleanly, list as enabled, evaluate on every message and do
    // nothing — permanently, and with no way to tell from the outside. Refusing
    // it here is the only place that failure is visible.
    if (form.actions.some((a) => a.type === "addLabel" && !(a.label ?? "").trim())) {
      pushToast({ kind: "error", message: t("rules.needsLabel") });
      return;
    }
    // ASKED BEFORE THE WRITE, NOT AFTER IT (#292): at 85 rules, a rule whose
    // conditions match an existing one usually means the sender is already
    // handled and the user is about to acquire a second rule for it. Skipped
    // when the list has not loaded — an advisory check that cannot run must not
    // become a save that cannot happen.
    if (!duplicateAcked && existing) {
      const dup = findDuplicateRule(
        { ...(form.id ? { id: form.id } : {}), conditions: form.conditions, actions: form.actions },
        existing,
        (target) => resolveTarget(target, state.labels)?.id,
      );
      if (dup) {
        setDuplicate(dup);
        return;
      }
    }
    await commit();
  };

  return (
    <div className="rule-form">
      <div className="rule-form-row">
        <label className="field">
          <span>{t("rules.name")}</span>
          <input
            type="text"
            value={form.name}
            placeholder={t("rules.namePlaceholder")}
            onChange={(e) => {
              // THE EVENT is what marks the name as the user's (#292) — not a
              // comparison against the default we last generated, which would
              // hand the field back to us the moment someone typed that default
              // themselves. Clearing the field counts as typing: they can name
              // it whatever they like, including nothing, and the save-time
              // "give the rule a name" check still applies.
              setNameAuto(false);
              setForm({ ...form, name: e.target.value });
            }}
          />
        </label>
        <label className="field field-narrow">
          <span>{t("rules.priority")}</span>
          <input
            type="number"
            value={form.priority}
            onChange={(e) => setForm({ ...form, priority: Number(e.target.value) || 0 })}
          />
          <small className="hint">{t("rules.priorityHint")}</small>
        </label>
      </div>

      <h3>{t("rules.conditions")}</h3>
      <ConditionNode node={form.conditions} onChange={setConditions} onRemove={null} depth={0} />

      <h3>{t("rules.actions")}</h3>
      <div className="rule-actions-list">
        {form.actions.map((a, i) => (
          <ActionRow
            key={i}
            action={a}
            labels={labels}
            onChange={(next) =>
              setForm((f) => ({ ...f, actions: f.actions.map((x, j) => (j === i ? next : x)) }))
            }
            onRemove={() =>
              setForm((f) => ({ ...f, actions: f.actions.filter((_, j) => j !== i) }))
            }
          />
        ))}
        <button
          type="button"
          className="link-button"
          onClick={() =>
            setForm((f) => ({ ...f, actions: [...f.actions, { type: "addLabel", label: "" }] }))
          }
        >
          + {t("rules.addAction")}
        </button>
      </div>

      <h3>{t("rules.preview.title")}</h3>
      <div className="rule-preview">
        <button type="button" className="secondary-button" onClick={() => void runPreview()} disabled={previewing}>
          {previewing ? t("rules.preview.checking") : t("rules.preview.check")}
        </button>
        {preview ? (
          <div className="rule-preview-result" aria-live="polite">
            {previewStale ? <p className="hint warn">{t("rules.preview.stale")}</p> : null}
            <p>
              {preview.matched === 0
                ? t("rules.preview.none")
                : preview.complete
                  ? t("rules.preview.exact", {
                      matched: preview.matched,
                      total: preview.total,
                    })
                  : t("rules.preview.bounded", {
                      matched: preview.matched,
                      scanned: preview.scanned,
                      total: preview.total,
                    })}
            </p>
            {preview.readBodies ? <p className="hint">{t("rules.preview.bodies")}</p> : null}
            {/* The sentence that stops a bad rule. Given its own emphasis, not
                folded into the count: "matches 47" and "…3 of which you filed
                elsewhere" are different facts, and only the second is a
                warning. */}
            {preview.conflicts > 0 ? (
              <p className="rule-conflicts" role="alert">
                {t("rules.preview.conflicts", {
                  count: preview.conflicts,
                  labels: preview.conflictLabels.join(", "),
                })}
              </p>
            ) : null}
            {preview.samples.length > 0 ? (
              <>
                <p className="hint">{t("rules.preview.samples")}</p>
                <ul className="rule-preview-samples">
                  {preview.samples.map((s) => (
                    <li key={s.id}>
                      <PreviewSample from={s.from} subject={s.subject} />
                    </li>
                  ))}
                </ul>
              </>
            ) : null}
          </div>
        ) : null}
      </div>

      {/* "You already have this rule" (#292), raised on Save rather than as a
          passive hint, because it is a decision and not a status: the user is
          one click from a second rule for a sender that is already handled.
          EDITING THE EXISTING ONE IS THE PRIMARY ACTION — at 85 rules that is
          what a collision almost always means, and it is the only one of the
          two that leaves the rules list smaller than it found it. */}
      {duplicate ? (
        <div className="rule-duplicate" role="alert">
          <h4>
            {duplicate.sameLabel ? t("rules.duplicate.same") : t("rules.duplicate.other")}
          </h4>
          <p className="rule-duplicate-rule">
            <span className="rule-name">{duplicate.rule.name}</span>
            <span className="rule-summary">{summarize(duplicate.rule, state.labels)}</span>
          </p>
          <p className="hint">
            {duplicate.sameLabel ? t("rules.duplicate.sameBody") : t("rules.duplicate.otherBody")}
          </p>
          <div className="rule-duplicate-actions">
            {onEditExisting ? (
              <button
                type="button"
                className="primary-button"
                onClick={() => onEditExisting(duplicate.rule)}
              >
                {t("rules.duplicate.edit")}
              </button>
            ) : null}
            <button
              type="button"
              className="secondary-button"
              disabled={saving}
              onClick={() => {
                // Answered once. Re-asking on the next Save click would make
                // this un-dismissable, and the user has already said yes.
                setDuplicateAcked(true);
                setDuplicate(null);
                void commit();
              }}
            >
              {t("rules.duplicate.createAnyway")}
            </button>
          </div>
        </div>
      ) : null}

      <footer className="rule-form-footer">
        <button type="button" className="secondary-button" onClick={onCancel}>
          {t("rules.cancel")}
        </button>
        <button type="button" className="primary-button" onClick={() => void save()} disabled={saving}>
          {t("rules.save")}
        </button>
      </footer>
    </div>
  );
}

// ---- the list --------------------------------------------------------------

/**
 * Resolve an addLabel target the way the SERVER does (store.resolveRuleLabel):
 * a label id, then a keyword atom, then a name. Mirroring that order matters —
 * checking ids alone would flag a perfectly good rule that names its label as
 * broken, and a false alarm about automation is worse than none.
 *
 * EXPORTED for day-one setup (#446), which asks `findDuplicateRule` the same
 * question about the rules it is about to propose and has to resolve targets the
 * same way to get the same answer. A second copy of this order is how the two
 * surfaces would start disagreeing about whether a rule already exists.
 */
export function resolveTarget(target: string, labels: Record<string, Label>): Label | undefined {
  const t = target.trim();
  if (!t) return undefined;
  const byId = labels[t];
  if (byId) return byId;
  return Object.values(labels).find((l) => l.keyword === t || l.name === t);
}

/**
 * Whether this rule applies a label that does not resolve (#166).
 *
 * The server SKIPS an unresolvable addLabel rather than failing the ingest, so
 * such a rule is silently inert: enabled, evaluated on every message, doing
 * nothing. It can arrive this way by being saved empty (no longer possible) or
 * by having its label deleted afterwards (always possible). Either way the list
 * is the only place it can be seen.
 */
function isInert(rule: Rule, labels: Record<string, Label>): boolean {
  return rule.actions.some(
    (a) => a.type === "addLabel" && !resolveTarget(a.label ?? "", labels),
  );
}

/** A one-line summary of what a rule does, for the list. */
function summarize(rule: Rule, labels: Record<string, Label>): string {
  const parts = rule.actions.map((a) => {
    if (a.type !== "addLabel") return actionLabel(a.type);
    const label = resolveTarget(a.label ?? "", labels);
    return `${actionLabel("addLabel")}: ${label?.name ?? a.label ?? "—"}`;
  });
  return parts.join(" · ");
}

/**
 * Run a saved rule over the mail already in the mailbox (#191).
 *
 * THE PREVIEW COMES FIRST, ALWAYS. This is the only control in the rules
 * surface that writes, and it can write to hundreds of messages on one click —
 * so it opens by asking the server what the rule matches, shows that, and waits.
 * Going straight to "applied to 412 messages" would be the confident bulk action
 * this project keeps deciding not to build.
 *
 * The RESULT then says what it did NOT do — what already had the label, what was
 * left alone because the user had corrected it, and which of the rule's actions
 * a backfill refuses. A backfill that silently performed three quarters of a
 * rule is worse than one that refused outright, because only the first kind
 * leaves you believing the rule ran.
 */
function ApplyPanel({
  rule,
  onDone,
}: {
  rule: Rule;
  onDone: () => void;
}): JSX.Element {
  const { actions, pushToast } = useApp();
  const mail = useMailState();
  const [preview, setPreview] = useState<RulePreview | null>(null);
  const [result, setResult] = useState<RuleApplyResult | null>(null);
  const [state, setState] = useState<"loading" | "ready" | "applying" | "done" | "failed">("loading");

  useEffect(() => {
    let live = true;
    void (async () => {
      const target = rule.actions.find((a) => a.type === "addLabel" && a.label)?.label;
      const res = await actions.previewRule(rule.conditions, target ? { targetLabelId: target } : {});
      if (!live) return;
      setPreview(res);
      setState(res ? "ready" : "failed");
    })();
    return () => {
      live = false;
    };
  }, [actions, rule.conditions, rule.actions]);

  const apply = async (): Promise<void> => {
    setState("applying");
    const res = await actions.applyRule(rule.id);
    if (!res) {
      setState("failed");
      return;
    }
    setResult(res);
    setState("done");
  };

  const undo = async (): Promise<void> => {
    if (!result || result.messageIds.length === 0) return;
    // An area-filing apply (#708) also hands back the desk/setup labels it
    // took off, so the undo can put those messages back where they were.
    const res = await actions.undoRuleApply(rule.id, result.messageIds, result.removedAreaLabels);
    if (!res) return;
    pushToast({ kind: "info", message: t("rules.apply.undone", { count: res.undone }) });
    onDone();
  };

  if (state === "loading") {
    return (
      <div className="rule-apply" aria-live="polite">
        <p className="hint">{t("rules.apply.checking")}</p>
      </div>
    );
  }
  if (state === "failed") {
    return (
      <div className="rule-apply" aria-live="polite">
        <p className="hint warn">{t("rules.apply.failed")}</p>
      </div>
    );
  }

  return (
    <div className="rule-apply" aria-live="polite">
      <h4>{t("rules.apply.title", { name: rule.name })}</h4>

      {state !== "done" && preview ? (
        <>
          <p>
            {preview.matched === 0
              ? t("rules.apply.none")
              : preview.complete
                ? t("rules.apply.matches", { matched: preview.matched, total: preview.total })
                : t("rules.apply.matchesBounded", {
                    matched: preview.matched,
                    scanned: preview.scanned,
                    total: preview.total,
                  })}
          </p>
          {/* The same warning the editor gives before SAVING a rule: matched mail
              the user already filed elsewhere by hand. It matters more here,
              because this writes immediately rather than on the next delivery. */}
          {preview.conflicts > 0 ? (
            <p className="rule-conflicts" role="alert">
              {t("rules.preview.conflicts", {
                count: preview.conflicts,
                labels: preview.conflictLabels.join(", "),
              })}
            </p>
          ) : null}
          {preview.areaFiling ? <AreaFilingLines filing={preview.areaFiling} areas={mail.areas} preview /> : null}
          {preview.samples.length > 0 ? (
            <ul className="rule-preview-samples">
              {preview.samples.slice(0, 8).map((s) => (
                <li key={s.id}>
                  <PreviewSample from={s.from} subject={s.subject} />
                </li>
              ))}
            </ul>
          ) : null}
          {preview.matched > 0 ? (
            <button
              type="button"
              className="primary-button"
              disabled={state === "applying"}
              onClick={() => void apply()}
            >
              {state === "applying"
                ? t("rules.apply.applying")
                : preview.areaFiling && preview.areaFiling.move > 0
                  ? t("rules.apply.area.confirm", {
                      count: preview.areaFiling.move,
                      area: mail.areas[preview.areaFiling.areaId]?.name ?? "",
                    })
                  : t("rules.apply.confirm", { count: preview.matched })}
            </button>
          ) : null}
        </>
      ) : null}

      {state === "done" && result ? (
        <>
          <p className="rule-apply-done">
            {result.changed > 0
              ? t("rules.apply.done", { count: result.changed })
              : t("rules.apply.nothingToDo")}
          </p>
          {result.alreadyDone > 0 ? (
            <p className="hint">{t("rules.apply.alreadyDone", { count: result.alreadyDone })}</p>
          ) : null}
          {result.areaFiling ? <AreaFilingLines filing={result.areaFiling} areas={mail.areas} /> : null}
          {/* Given its own emphasis, not folded into a tally. "We did not undo
              your corrections" is a promise being kept, and the user has no
              other way to know it was even considered. */}
          {result.skippedCorrected > 0 ? (
            <p className="rule-apply-respected">
              {t("rules.apply.skippedCorrected", { count: result.skippedCorrected })}
            </p>
          ) : null}
          {result.skippedActions.length > 0 ? (
            <p className="hint">
              {t("rules.apply.skippedActions", { actions: result.skippedActions.join(", ") })}
            </p>
          ) : null}
          {result.truncated ? (
            <p className="hint warn">
              {t("rules.apply.truncated", { changed: result.changed, remaining: result.remaining })}
            </p>
          ) : null}
          {result.messageIds.length > 0 ? (
            <button type="button" className="secondary-button" onClick={() => void undo()}>
              {t("rules.apply.undo")}
            </button>
          ) : null}
        </>
      ) : null}
    </div>
  );
}

/**
 * What filing into an area did, or would do (#708) — one line per outcome, and
 * the mail that stays put given its own emphasis: "your own filing was kept" is
 * a promise, and a count folded into a total would not show it was kept.
 */
function AreaFilingLines({
  filing,
  areas,
  preview = false,
}: {
  filing: RuleAreaFiling;
  areas: Record<string, { name: string }>;
  preview?: boolean;
}): JSX.Element {
  const area = areas[filing.areaId]?.name ?? "";
  const reasons: string[] = [];
  if (filing.contestedByUser > 0) reasons.push(t("rules.apply.area.byUser", { count: filing.contestedByUser }));
  if (filing.contestedByRule > 0) reasons.push(t("rules.apply.area.byRule", { count: filing.contestedByRule }));
  if (filing.contestedOther > 0) reasons.push(t("rules.apply.area.byOther", { count: filing.contestedOther }));
  const contested = filing.contestedByUser + filing.contestedByRule + filing.contestedOther;
  return (
    <>
      {filing.move > 0 ? (
        <p className="rule-apply-area">
          {preview
            ? t("rules.apply.area.willMove", { count: filing.move, area })
            : t("rules.apply.area.moved", { count: filing.move, area })}
        </p>
      ) : null}
      {filing.alreadyThere > 0 ? (
        <p className="hint">{t("rules.apply.area.alreadyThere", { count: filing.alreadyThere, area })}</p>
      ) : null}
      {contested > 0 ? (
        <p className="rule-apply-respected" {...(preview ? { role: "alert" } : {})}>
          {t("rules.apply.area.contested", { count: contested, reasons: reasons.join("; ") })}
        </p>
      ) : null}
    </>
  );
}

/**
 * The offer to fix a rule the user keeps correcting (#170).
 *
 * It opens where the complaint is — inline, under the rule's own row — because
 * "this rule has misfiled 4 messages" and "here is a rule that would not have"
 * are one thought, and putting the second behind a different screen is how a
 * diagnosis becomes a nag.
 *
 * NOTHING IS SAVED FROM HERE. Accepting loads the narrower conditions into the
 * ordinary editor, where the same blast radius, the same validation and the same
 * Save button apply. A feature that rewrites your rules the moment you agree
 * with it is one you stop agreeing with.
 */
function TightenPanel({
  rule,
  onAccept,
}: {
  rule: Rule;
  onAccept: (conditions: RuleCondition) => void;
}): JSX.Element {
  const { actions } = useApp();
  const [result, setResult] = useState<RuleTightenResult | null>(null);
  const [state, setState] = useState<"loading" | "ready" | "failed">("loading");

  useEffect(() => {
    let live = true;
    void (async () => {
      const res = await actions.tightenRule(rule.id);
      if (!live) return;
      setResult(res);
      setState(res ? "ready" : "failed");
    })();
    return () => {
      live = false;
    };
  }, [actions, rule.id]);

  if (state === "loading") {
    return (
      <div className="rule-tighten" aria-live="polite">
        <p className="hint">{t("rules.corrections.checking")}</p>
      </div>
    );
  }
  if (state === "failed" || !result) {
    return (
      <div className="rule-tighten" aria-live="polite">
        <p className="hint warn">{t("rules.corrections.failed")}</p>
      </div>
    );
  }

  const best = result.proposals[0];
  return (
    <div className="rule-tighten" aria-live="polite">
      <h4>{t("rules.corrections.title", { name: rule.name })}</h4>
      <p>
        {t("rules.corrections.current", {
          matched: result.current.matched,
          keeps: result.current.keeps,
          misfires: result.current.misfires,
          collateral: result.current.collateral,
        })}
      </p>
      {/* The window qualifier travels with every number on this panel. A count
          measured over recent mail must never be read as a whole-mailbox fact —
          the same contract Rule/preview holds itself to. */}
      {result.complete ? null : (
        <p className="hint">
          {t("rules.corrections.bounded", { scanned: result.scanned, total: result.total })}
        </p>
      )}

      {/* Three distinct answers, three different sentences. Collapsing "no rule
          fits" into "we could not check" would tell the user to wait for
          something that is never coming. */}
      {!best && result.correctionsInWindow === 0 ? (
        <p className="hint">{t("rules.corrections.outOfWindow")}</p>
      ) : null}
      {!best && result.correctionsInWindow > 0 ? (
        <p className="hint">{t("rules.corrections.none")}</p>
      ) : null}

      {best ? (
        <>
          <p className="rule-tighten-proposal">
            {t("rules.corrections.proposal", {
              matched: best.matched,
              keeps: best.keeps,
              fixes: best.fixes,
            })}
          </p>
          {/* THE PRICE, stated even when it is zero. A narrower rule that quietly
              stops labelling mail the user had kept is the failure this whole
              feature is meant to avoid repeating. */}
          {best.drops > 0 ? (
            <>
              <p className="rule-tighten-drops" role="alert">
                {t("rules.corrections.drops", { count: best.drops })}
              </p>
              <ul className="rule-preview-samples">
                {best.samples.map((s) => (
                  <li key={s.id}>
                    <PreviewSample from={s.from} subject={s.subject} />
                  </li>
                ))}
              </ul>
            </>
          ) : (
            <p className="hint">{t("rules.corrections.dropsNone")}</p>
          )}
          <button type="button" className="primary-button" onClick={() => onAccept(best.conditions)}>
            {t("rules.corrections.review")}
          </button>
        </>
      ) : null}
    </div>
  );
}

/**
 * THE RULES ROW OF THE SETTINGS PAGE (#374 increment 2) — was `RulesSheet`, a
 * modal with its own ✕.
 *
 * The content is unchanged: the list, the enable checkbox, the inert and
 * corrections badges, the "apply to existing mail" and "narrow it" panels, and
 * the full editor. What went is the dialog furniture — the row's heading opens
 * this and Escape peels the settings route (app/routing.ts), so a second closing
 * affordance inside the row would be a second way to do one thing.
 *
 * `initialDraft` is still how "create a rule from this message" arrives
 * (ui/shellContext.ts openRuleDraft): it navigates to `#/settings/rules`, which
 * is now a row expanded rather than a sheet opened, and the draft is handed
 * straight to the form.
 */
export function RulesBody({ initialDraft }: { initialDraft?: RuleDraft }): JSX.Element {
  const { actions, pushToast } = useApp();
  const state = useMailState();
  const [rules, setRules] = useState<Rule[] | null>(null);
  const [failed, setFailed] = useState(false);
  // Opening straight into a draft is the "create a rule from this message" path.
  const [editing, setEditing] = useState<RuleDraft | null>(initialDraft ?? null);
  // Which rule's "narrow it" panel is open (#170). One at a time: each costs a
  // server-side scan, and two open at once would be two answers competing for
  // the same decision.
  const [tightening, setTightening] = useState<string | null>(null);
  // Which rule's "apply to existing mail" panel is open (#191). One at a time,
  // and never at the same time as a tighten panel: they ask opposite questions
  // about the same rule — "do more of this" and "do less of this" — and both
  // open at once is two answers competing for one decision.
  const [applying, setApplying] = useState<string | null>(null);

  const reload = useCallback(async (): Promise<void> => {
    const rs = await actions.listRules();
    if (rs === null) {
      setFailed(true);
      return;
    }
    setFailed(false);
    setRules(rs);
  }, [actions]);

  useEffect(() => {
    void reload();
  }, [reload]);

  const remove = async (rule: Rule): Promise<void> => {
    if (!window.confirm(t("rules.deleteConfirm", { name: rule.name }))) return;
    if (await actions.deleteRule(rule.id)) {
      pushToast({ kind: "info", message: t("rules.deleted") });
      void reload();
    }
  };

  return (
    <>
      {editing ? (
        <RuleForm
          // KEYED, so that switching the form to a different rule REMOUNTS it
          // (#292). RuleForm seeds its own state from `draft` once, with
          // useState — which was safe while setEditing was only ever called
          // from the closed list. "Edit the existing rule instead" calls it
          // while the form is open, and without a key the user would click it
          // and watch nothing change.
          key={editing.id ?? "new"}
          draft={editing}
          // Already loaded for the list below — duplicate detection costs no
          // extra JMAP call. Null while loading, which SKIPS the check.
          existing={rules}
          onEditExisting={(rule) =>
            setEditing({
              id: rule.id,
              name: rule.name,
              priority: rule.priority,
              conditions: rule.conditions,
              actions: rule.actions,
              enabled: rule.enabled,
            })
          }
          onCancel={() => setEditing(null)}
          onDone={() => {
            setEditing(null);
            void reload();
          }}
        />
      ) : (
        <>
          <div className="rule-list-header">
            <button
              type="button"
              className="primary-button"
              onClick={() =>
                setEditing({
                  name: "",
                  priority: 100,
                  conditions: { all: [newLeaf()] },
                  actions: [{ type: "addLabel", label: "" }],
                  enabled: true,
                  // A rule written from scratch names itself too (#292), from
                  // whatever the first condition ends up saying plus the label.
                  // There is nothing to overwrite — the field starts empty —
                  // and the first keystroke in it hands the name back.
                  nameAuto: true,
                })
              }
            >
              + {t("rules.new")}
            </button>
          </div>

          {failed ? <p className="empty">{t("rules.loadFailed")}</p> : null}
          {!failed && rules === null ? <p className="empty">{t("rules.loading")}</p> : null}
          {rules !== null && rules.length === 0 ? <p className="empty">{t("rules.none")}</p> : null}

          {rules !== null && rules.length > 0 ? (
            <>
              <p className="hint">{t("rules.evaluationOrder")}</p>
              <ul className="rule-list">
                {rules.map((r) => (
                  <li key={r.id} className={r.enabled ? "" : "is-disabled"}>
                    <div className="rule-row">
                      <label className="checkbox">
                        <input
                          type="checkbox"
                          checked={r.enabled}
                          aria-label={r.enabled ? t("rules.enabled") : t("rules.disabled")}
                          onChange={() => {
                            void (async () => {
                              if (await actions.updateRule(r.id, { enabled: !r.enabled })) {
                                void reload();
                              }
                            })();
                          }}
                        />
                        <span className="rule-name">{r.name}</span>
                      </label>
                      <span className="rule-summary">
                        {summarize(r, state.labels)}
                        {/* Only worth saying for an ENABLED rule: a disabled one
                            is already understood to be doing nothing. */}
                        {r.enabled && isInert(r, state.labels) ? (
                          <span className="rule-inert" title={t("rules.inert")}>
                            {t("rules.inert")}
                          </span>
                        ) : null}
                        {/* Shown for a DISABLED rule too, unlike the inert
                            warning: turning a misfiring rule off is how people
                            work around it, and hiding the count then would erase
                            the record of why it is off. */}
                        {r.corrections > 0 ? (
                          <span
                            className="rule-misfiled"
                            title={
                              r.corrections === 1
                                ? t("rules.corrections.one")
                                : t("rules.corrections.many", { count: r.corrections })
                            }
                          >
                            {t("rules.corrections.badge", { count: r.corrections })}
                          </span>
                        ) : null}
                      </span>
                      <span className="rule-row-actions">
                        {/* Only for an ENABLED rule: the server refuses a
                            disabled one, and offering a button that always
                            errors is worse than not offering it. */}
                        {r.enabled ? (
                          <button
                            type="button"
                            className="link-button"
                            aria-expanded={applying === r.id}
                            onClick={() => {
                              setTightening(null);
                              setApplying(applying === r.id ? null : r.id);
                            }}
                          >
                            {t("rules.apply.button")}
                          </button>
                        ) : (
                          <span className="hint">{t("rules.apply.disabledHint")}</span>
                        )}
                        {r.corrections > 0 ? (
                          <button
                            type="button"
                            className="link-button"
                            aria-expanded={tightening === r.id}
                            onClick={() => {
                              setApplying(null);
                              setTightening(tightening === r.id ? null : r.id);
                            }}
                          >
                            {t("rules.corrections.narrow")}
                          </button>
                        ) : null}
                        <button
                          type="button"
                          className="link-button"
                          onClick={() =>
                            setEditing({
                              id: r.id,
                              name: r.name,
                              priority: r.priority,
                              conditions: r.conditions,
                              actions: r.actions,
                              enabled: r.enabled,
                            })
                          }
                        >
                          {t("rules.edit")}
                        </button>
                        <button type="button" className="link-button" onClick={() => void remove(r)}>
                          {t("rules.delete")}
                        </button>
                      </span>
                    </div>
                    {tightening === r.id ? (
                      <TightenPanel
                        rule={r}
                        onAccept={(conditions) => {
                          setTightening(null);
                          setEditing({
                            id: r.id,
                            name: r.name,
                            priority: r.priority,
                            conditions,
                            actions: r.actions,
                            enabled: r.enabled,
                          });
                        }}
                      />
                    ) : null}
                    {applying === r.id ? (
                      <ApplyPanel
                        rule={r}
                        onDone={() => {
                          setApplying(null);
                          // Re-read the rules: an apply can move a rule's
                          // corrections count (a backfill skips corrected mail
                          // but an undo does not touch it), and a stale badge
                          // beside a rule the user just acted on is the kind of
                          // thing that makes a number look untrustworthy.
                          void reload();
                        }}
                      />
                    ) : null}
                  </li>
                ))}
              </ul>
            </>
          ) : null}
        </>
      )}
    </>
  );
}
