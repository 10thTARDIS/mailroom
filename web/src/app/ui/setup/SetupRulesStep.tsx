// SPDX-License-Identifier: AGPL-3.0-only

// STEP 3 OF 4 — RULES, LEARNED FROM YOUR FOLDERS (#446).
//
// The folders tell Mailroom who sends what. That is evidence about what a person
// wants in exactly the way a hand-applied label is — and on the day a mailbox
// arrives from another client it is the ONLY evidence there is, which is the day a
// rule is worth the most.
//
// EVERY PROPOSAL SHOWS WHAT IT WOULD HAVE DONE TO THE MAIL THAT IS ALREADY THERE,
// and it shows it as a pair: "would have placed 320 / 320". A ratio without its
// denominator is not a measurement, and a proposal is worth accepting exactly to
// the extent that the mail already in the folder agrees with it. Where the scan
// was BOUNDED the row says so ("measured over the 2,000 most recent of 6,510"):
// a bounded count that reads as a total is #155, and the whole point of this step
// is that the numbers can be trusted.
//
// NOTHING HERE IS A NEW WRITE PATH. Accept goes through `actions.createRule` —
// the same call the rules editor makes, with the same name generator
// (`autoRuleName`), the same duplicate check (`findDuplicateRule`) and the same
// server-side retry when a name is taken. A second create path for rules is how
// two surfaces start disagreeing about what a rule is.

import { useEffect, useState } from "react";
import type { JSX } from "react";

import type { FolderCensus, InducedRule, Rule, RuleInduceResult } from "../../../core/jmap/types";
import { autoRuleName, findDuplicateRule } from "../../../core/ruleIdentity";
import { useApp, useMailState } from "../../state";
import { t } from "../../i18n";
import { resolveTarget } from "../RuleEditor";
import { SetupFooter, StepBody } from "./SetupRoute";
import type { StepProps } from "./SetupRoute";
import { choiceFor } from "./setupState";
import { countText, shapeSentence } from "./sentences";
import {
  ACCEPT_ALL_THRESHOLD,
  acceptable,
  candidateKey,
  coverageText,
  describeConditions,
  isAccepted,
} from "./rules";

/** How much mail one induction is measured over. The server clamps it
 * (previewMaxScan) and reports what it actually scanned, which is what the row
 * renders — this is the ceiling asked for, not a promise about the answer. */
const INDUCE_LIMIT = 2000;

/** One folder's answer: the folder, the label its mail is getting, and what the
 * server said. `result: null` is "could not be asked", which is not the same as
 * "no clean rule" and does not read the same. */
interface Proposal {
  folder: FolderCensus;
  labelName: string;
  result: RuleInduceResult | null;
}

export function SetupRulesStep(props: StepProps): JSX.Element {
  const { state, dispatch, census, onNext, onBack, onSkip } = props;
  const { actions } = useApp();
  const mail = useMailState();

  const [proposals, setProposals] = useState<Proposal[] | null>(null);
  /** The rules that already exist, for the duplicate check. `null` while it has
   * not loaded — and an advisory check that cannot run must never become an accept
   * that cannot happen (RuleEditor makes the same call and says the same). */
  const [existing, setExisting] = useState<Rule[] | null>(null);
  const [busy, setBusy] = useState(false);

  /**
   * WHICH FOLDERS ARE ASKED ABOUT: the ones the plan is actually acting on, and
   * never INBOX.
   *
   * A `leave` folder is excluded because the user deferred it — inducing a rule
   * from mail they have not imported would file future mail into an area that
   * holds none of the past. INBOX is excluded because its mail is not evidence of
   * anything: it is the pile that has not been sorted.
   */
  const learnable = census.filter(
    (f) => f.role !== "inbox" && choiceFor(state, f).action !== "leave",
  );
  const learnableKey = learnable.map((f) => f.mailboxId).join(",");

  useEffect(() => {
    let cancelled = false;
    setProposals(null);
    void (async () => {
      const rules = await actions.listRules();
      if (!cancelled) setExisting(rules);
      // Sequential rather than Promise.all: each induction is a scan of up to
      // 2,000 messages server-side, and firing one per folder at once on a
      // twelve-folder mailbox is a thundering herd against the user's own server
      // for a screen they read top to bottom anyway.
      const out: Proposal[] = [];
      for (const folder of learnable) {
        const result = await actions.induceRuleForMailbox(folder.mailboxId, INDUCE_LIMIT);
        if (cancelled) return;
        out.push({ folder, labelName: labelFor(folder, state), result });
        setProposals([...out]);
      }
      if (!cancelled) setProposals(out);
    })();
    return () => {
      cancelled = true;
    };
    // Keyed on WHICH folders, not on the state object: editing a label in step 1
    // must not re-scan the mailbox.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [actions, learnableKey]);

  /** Creates the rule behind one proposal, unless this pass already has. */
  const accept = async (proposal: Proposal, candidate: InducedRule): Promise<boolean> => {
    const key = candidateKey(candidate.conditions, proposal.labelName);
    if (state.acceptedRules.includes(key)) return false;
    // The rule THE AREA'S OWN LABEL is the placement: applying the folder's label
    // is what puts the mail in its area (#442, "areas are labels"), so there is no
    // second action to add here.
    const created = await actions.createRule({
      name: autoRuleName(
        { label: proposal.labelName, conditions: candidate.conditions },
        (k, label) => t("rules.autoName", { key: k, label }),
      ),
      priority: 100,
      conditions: candidate.conditions,
      actions: [{ type: "addLabel", label: proposal.labelName }],
      enabled: true,
    });
    if (created === null) return false;
    dispatch({ type: "acceptRule", key });
    return true;
  };

  /**
   * "Accept all with a preview above 90%".
   *
   * It accepts only what CLEARS THE BAR — `acceptable` is false with no
   * denominator, so a folder the server could not measure is never swept up in a
   * bulk accept — and it skips anything already accepted or already existing as a
   * rule, so pressing it twice creates nothing the second time.
   */
  const acceptAll = (): void => {
    if (!proposals) return;
    setBusy(true);
    void (async () => {
      for (const proposal of proposals) {
        const result = proposal.result;
        if (!result) continue;
        for (const candidate of result.candidates) {
          if (!acceptable({ covers: candidate.covers, examples: result.examples }, ACCEPT_ALL_THRESHOLD)) {
            continue;
          }
          if (duplicateOf(proposal, candidate, existing, mail.labels) !== null) continue;
          await accept(proposal, candidate);
        }
      }
      setBusy(false);
    })();
  };

  const sweepable =
    proposals?.reduce((n, p) => {
      const result = p.result;
      if (!result) return n;
      return (
        n +
        result.candidates.filter(
          (c) =>
            acceptable({ covers: c.covers, examples: result.examples }, ACCEPT_ALL_THRESHOLD) &&
            !isAccepted(state.acceptedRules, c.conditions, p.labelName) &&
            duplicateOf(p, c, existing, mail.labels) === null,
        ).length
      );
    }, 0) ?? 0;

  return (
    <StepBody>
      <p className="setup-lead">{t("setup.rules.lead")}</p>

      {learnable.length === 0 ? (
        <p className="field-hint">{t("setup.rules.nothing")}</p>
      ) : proposals === null ? (
        <p className="setup-note" role="status">
          {t("setup.rules.looking")}
        </p>
      ) : (
        <>
          <ul className="setup-proposals">
            {proposals.map((proposal) => (
              <ProposalRows
                key={proposal.folder.mailboxId}
                proposal={proposal}
                accepted={state.acceptedRules}
                existing={existing}
                busy={busy}
                onAccept={(candidate) => {
                  setBusy(true);
                  void accept(proposal, candidate).finally(() => setBusy(false));
                }}
              />
            ))}
          </ul>
          <div className="setup-dial-row">
            <button
              type="button"
              className="button button-quiet"
              // Disabled when there is nothing it would do, rather than enabled and
              // silently doing nothing — a button that reports success for zero work
              // is the shape of failure this project keeps paying for.
              disabled={busy || sweepable === 0}
              onClick={acceptAll}
            >
              {t("setup.rules.acceptAll")}
            </button>
            {sweepable > 0 ? (
              <span className="setup-dial-note">{t("setup.rules.acceptN", { n: sweepable })}</span>
            ) : null}
          </div>
        </>
      )}

      <SetupFooter
        onBack={onBack}
        onSkip={onSkip}
        next={{ label: t("setup.next.4"), onClick: onNext, disabled: busy }}
      />
    </StepBody>
  );
}

/** The label a folder's mail is getting — the same one step 1 proposed or the user
 * edited, falling back to the folder's own name exactly as the server does. */
function labelFor(folder: FolderCensus, state: StepProps["state"]): string {
  const chosen = choiceFor(state, folder).labelName.trim();
  return chosen === "" ? folder.name : chosen;
}

/** Whether a rule matching this proposal already exists — asked through the same
 * function, with the same label resolution, the rules editor uses. Null when the
 * rule list has not loaded, which is "not asked" and never "no". */
function duplicateOf(
  proposal: Proposal,
  candidate: InducedRule,
  existing: Rule[] | null,
  labels: ReturnType<typeof useMailState>["labels"],
): Rule | null {
  if (!existing) return null;
  const found = findDuplicateRule(
    {
      conditions: candidate.conditions,
      actions: [{ type: "addLabel", label: proposal.labelName }],
    },
    existing,
    (target) => resolveTarget(target, labels)?.id,
  );
  return found?.rule ?? null;
}

/** One folder's rows: a line per candidate, or the one honest line for a folder
 * that implies no rule. */
function ProposalRows({
  proposal,
  accepted,
  existing,
  busy,
  onAccept,
}: {
  proposal: Proposal;
  accepted: readonly string[];
  existing: Rule[] | null;
  busy: boolean;
  onAccept: (candidate: InducedRule) => void;
}): JSX.Element {
  const mail = useMailState();
  const { folder, labelName, result } = proposal;
  const folderName = t("setup.rules.folder", { name: folder.name });

  if (result === null) {
    return (
      <li className="setup-proposal">
        <span className="setup-proposal-head">
          <strong>{folderName}</strong>
        </span>
        <span className="setup-proposal-numbers">{t("setup.rules.unmeasured")}</span>
      </li>
    );
  }

  if (result.candidates.length === 0) {
    return (
      <li className="setup-proposal">
        <span className="setup-proposal-head">
          <strong>{folderName}</strong>
          <span className="setup-proposal-numbers">
            {/* A folder with too few examples is a different answer from a folder
                whose mail has nothing in common, and only one of the two is a dead
                end. Both say what was measured. */}
            {result.examples < result.required
              ? t("setup.rules.needMore", {
                  examples: countText(result.examples),
                  required: countText(result.required),
                })
              : t("setup.rules.none")}
          </span>
        </span>
        <span className="setup-proposal-numbers">{sendersNote(folder)}</span>
      </li>
    );
  }

  return (
    <>
      {result.candidates.map((candidate) => {
        const already = isAccepted(accepted, candidate.conditions, labelName);
        const duplicate = duplicateOf(proposal, candidate, existing, mail.labels);
        const short = Math.max(0, result.examples - candidate.covers);
        const phrases = describeConditions(candidate.conditions);
        return (
          <li key={candidateKey(candidate.conditions, labelName)} className="setup-proposal">
            <span className="setup-proposal-head">
              {/* "Sender domain is creators.example → Creators": the rule's leading
                  condition and where its mail lands, which is the whole proposal in
                  one line. The conjuncts are listed below when there is more than
                  one — repeating a single one would be saying it twice. */}
              <strong>
                {phrases[0] ?? labelName} {t("setup.rules.into", { area: labelName })}
              </strong>
              <span className="setup-proposal-numbers">{folderName}</span>
              <span className="setup-proposal-numbers">{coverageText(result, candidate)}</span>
              {short > 0 ? (
                <span className="setup-proposal-numbers">
                  {t("setup.rules.short", { n: countText(short) })}
                </span>
              ) : null}
              {/* A BOUNDED SCAN REPORTS ITS BOUND (#155). */}
              {!result.complete ? (
                <span className="setup-proposal-numbers">
                  {t("setup.rules.bounded", {
                    scanned: countText(result.scanned),
                    total: countText(result.total),
                  })}
                </span>
              ) : null}
              {already || duplicate !== null ? (
                // A WORD, not a disabled button with a tick: "Accepted" is a state
                // that has to be readable, and a control nobody can press is not
                // how to say it.
                <span className="setup-accepted">{t("setup.rules.accepted")}</span>
              ) : (
                <button
                  type="button"
                  className="button button-quiet"
                  disabled={busy}
                  onClick={() => onAccept(candidate)}
                >
                  {t("setup.rules.accept")}
                </button>
              )}
            </span>
            {/* The conditions in words, ONE PER CONJUNCT — the same vocabulary the
                rules editor labels its own selects with. An AND of three tests is
                three things to check against the folder in front of you. */}
            {phrases.length > 1 ? (
              <ul className="setup-proposal-conditions">
                {phrases.map((phrase) => (
                  <li key={phrase}>{phrase}</li>
                ))}
              </ul>
            ) : null}
          </li>
        );
      })}
    </>
  );
}

/**
 * What the folder's senders look like, for a folder that implies no rule.
 *
 * "no clean rule" is a CONCLUSION, and a conclusion with no numbers under it is a
 * verdict. `shapeSentence` already carries the counts for the shapes derived from
 * one ("5 senders cover 190", "bulk mail · 38 senders"); `mixed` is the one word
 * that does not, so the sender count is put beside it — "22 senders, mixed" is the
 * design's own phrasing and it is the fact that makes the conclusion checkable.
 */
function sendersNote(folder: FolderCensus): string {
  if (folder.shape !== "mixed") return shapeSentence(folder);
  return t("setup.rules.mixedSenders", { n: countText(folder.distinctSenders) });
}
