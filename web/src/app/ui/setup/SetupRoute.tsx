// SPDX-License-Identifier: AGPL-3.0-only

// DAY ONE (#446): the four steps that turn a folder-sorted mailbox into a desk.
//
// WHAT THE WHOLE SURFACE PROMISES, and every part of it is load-bearing:
//
//   * every step shows a REPORT before it does anything;
//   * NOTHING MOVES ON THE SERVER unless the user chooses it, folder by folder;
//   * each step is RESUMABLE — the state is persisted per login, so a reload or a
//     closed tab comes back to the plan that was built rather than to step 1;
//   * any folder can be imported later, from Settings → Areas and drawers, which
//     is what makes "Leave alone" a real answer rather than a dead end.
//
// THIS FILE IS THE FRAME AND THE DATA, NOT THE STEPS. It owns the census (one
// request, see below), the reducer, the persistence and the navigation; each step
// is a component that reads the state and renders its own controls and its own
// footer, because the four steps have four different Next buttons doing four
// different things.
//
// ⚠ ONE CENSUS FOR EVERY ACCOUNT, NOT ONE PER ACCOUNT. #446 describes "one census
// per account" for a multi-account login; this asks `Setup/census` WITHOUT
// `mailAccountId` and groups the rows by `FolderCensus.accountId`, which the
// server puts on every row for exactly this purpose. The data is identical, it is
// one round trip instead of N, and — the reason it is not merely cheaper — every
// row is then counted at the same window at the same instant. Two censuses taken
// a second apart would put two accounts' numbers under one sentence and say they
// were measured together.
//
// ⚠ THE CENSUS IS RE-ASKED WHEN THE WINDOW MOVES, rather than the client
// re-deriving `inWindow` from a count it did not compute. The census is cheap (one
// read-only query per folder) and the server's counts are the only truth about
// which side of a cutoff a message falls on.

import { useCallback, useEffect, useMemo, useReducer, useRef, useState } from "react";
import type { ReactNode } from "react";
import type { JSX } from "react";

import type { FolderCensus, Id, SetupCensusResult } from "../../../core/jmap/types";
import type { SetupStep } from "../../../core/routes";
import { navigate, useRoute } from "../../routing";
import { useApp, useMailState } from "../../state";
import { t } from "../../i18n";
import type { MessageKey } from "../../i18n";
import {
  clearSetupState,
  initialSetupState,
  loadSetupState,
  saveSetupState,
  setupReducer,
} from "./setupState";
import type { SetupEdit, SetupState } from "./setupState";
import { browserStorage } from "./setupStorage";
import { SetupCensusStep } from "./SetupCensusStep";
import { SetupMailStep } from "./SetupMailStep";
import { SetupRulesStep } from "./SetupRulesStep";
import { SetupDialsStep } from "./SetupDialsStep";
import "../../setup.css";

/** The storage the wizard's draft lives in. Module-level so every render hands
 * the same object to the reducer's persistence effect. */
const draftStore = browserStorage("local");

/** What every step is handed. One object rather than eleven props, because all
 * four steps need most of it and a step that quietly stopped being given the
 * census would render a table of nothing. */
export interface StepProps {
  state: SetupState;
  dispatch: (edit: SetupEdit) => void;
  /** Every folder day-one can act on, at the CURRENT window. */
  census: FolderCensus[];
  /** The window those counts were taken at, as the SERVER reports it — not the
   * one the client asked for. They agree today; reading the server's is what
   * keeps the sentence true if they ever do not. */
  windowDays: number;
  /** Import mode: the one folder, or null for the whole wizard. */
  importFolder: FolderCensus | null;
  onNext: () => void;
  onBack: (() => void) | null;
  onSkip: () => void;
  /** Leaves the wizard for good: writes `setupCompletedAt`, forgets the draft and
   * opens the desk. Step 4's "Open the desk" and step 1's "Skip setup" are the
   * same exit with different preferences written on the way out. */
  onFinish: (patch?: Record<string, unknown>) => Promise<void>;
}

export function SetupRoute(): JSX.Element | null {
  const route = useRoute();
  const { actions, sync } = useApp();
  const mail = useMailState();
  const loginId = mail.accountId;

  const step: SetupStep = route.page === "setup" ? route.step : 1;
  const importMailboxId: Id | null =
    route.page === "setup" ? route.importMailboxId ?? null : null;

  // The draft, loaded once per login. `useReducer` with an initialiser rather
  // than an effect, so the very first render already has the plan the user built
  // instead of flashing the defaults and correcting.
  const [state, dispatch] = useReducer(
    setupReducer,
    loginId,
    (id): SetupState => (id ? loadSetupState(draftStore, id) : initialSetupState()),
  );

  useEffect(() => {
    if (!loginId) return;
    saveSetupState(draftStore, loginId, state);
  }, [loginId, state]);

  /** `undefined` while the census is in flight, `null` when it could not be
   * taken, and the answer otherwise. Three states, because "you have no folders"
   * and "we could not look" must not read the same (#176). */
  const [census, setCensus] = useState<SetupCensusResult | null | undefined>(undefined);
  const [reloads, setReloads] = useState(0);

  useEffect(() => {
    if (!loginId) return;
    let cancelled = false;
    setCensus(undefined);
    void actions.setupCensus({ windowDays: state.windowDays }).then((res) => {
      if (!cancelled) setCensus(res);
    });
    return () => {
      cancelled = true;
    };
    // `reloads` is the retry button; the window is what changes the counts.
  }, [actions, loginId, state.windowDays, reloads]);

  const folders = census?.folders ?? [];
  const importFolder = useMemo(
    () => (importMailboxId ? folders.find((f) => f.mailboxId === importMailboxId) ?? null : null),
    [folders, importMailboxId],
  );

  // FOCUS MOVES TO THE STEP HEADING when the step changes (increment A's rules).
  // Without it, Next leaves the focus on a button that no longer exists and a
  // screen reader is told nothing at all about the screen having changed.
  const headingRef = useRef<HTMLHeadingElement>(null);
  useEffect(() => {
    headingRef.current?.focus();
  }, [step]);

  const goTo = useCallback(
    (next: SetupStep) => {
      navigate({ page: "setup", step: next, ...(route.acct ? { acct: route.acct } : {}) });
    },
    [route.acct],
  );

  /**
   * The one exit, used by "Skip setup, open the desk" and by "Open the desk".
   *
   * ⚠ THE DRAFT IS FORGOTTEN ONLY AFTER THE WRITE LANDS. A cleared draft plus a
   * refused `setupCompletedAt` would be the worst of both: the wizard opens again
   * on the next load AND the plan the user built is gone.
   */
  const finish = useCallback(
    async (patch: Record<string, unknown> = {}) => {
      const ok = await actions.updateDeskPreferences(
        { ...patch, setupCompletedAt: new Date().toISOString() },
        "Could not finish setting up",
      );
      if (!ok) return;
      if (loginId) clearSetupState(draftStore, loginId);
      // THE DESK, not wherever the user happened to be when the wizard opened
      // itself. Both buttons that come through here say "open the desk", and
      // `#/inbox` IS the desk (#375) — landing back on a search or an all-mail
      // view because that was the last list would be answering a different
      // sentence. Escape is the one that goes back where you were.
      navigate({ page: "inbox", tab: null, ...(route.acct ? { acct: route.acct } : {}) });
      // The desk has to be re-asked: the run has labelled and archived thousands
      // of messages and the line may have moved, so every section and every count
      // on the screen we are about to show is stale.
      void sync.refreshDesk();
    },
    [actions, loginId, route.acct, sync],
  );

  const shared = {
    state,
    dispatch,
    census: folders,
    windowDays: census?.windowDays ?? state.windowDays,
    importFolder,
    onSkip: () => void finish(),
    onFinish: finish,
  };

  return (
    <main className="content setup-content" id="messages" tabIndex={-1}>
      <section className="setup" aria-labelledby="setup-title">
        <header className="setup-head">
          <h2 id="setup-title">{t("setup.title")}</h2>
          {/* THE STEP LIST IS A REAL LIST with `aria-current="step"` on the one we
              are on, so the position in the wizard is available without sight and
              without colour. The numbers are not links: a step ahead of the one
              you are on has not been decided yet. */}
          <ol className="setup-steps" aria-label={t("setup.steps")}>
            {([1, 2, 3, 4] as SetupStep[]).map((n) => (
              <li
                key={n}
                className={`setup-step${n === step ? " is-current" : ""}`}
                aria-current={n === step ? "step" : undefined}
              >
                <span className="setup-step-n">{t("setup.step", { n })}</span>{" "}
                <span className="setup-step-name">{t(`setup.step.${n}` as MessageKey)}</span>
              </li>
            ))}
          </ol>
          {/* `tabIndex={-1}` so focus can be MOVED here on a step change without
              putting the heading in the tab order. */}
          <h3 className="setup-step-heading" ref={headingRef} tabIndex={-1}>
            {t(`setup.step.${step}` as MessageKey)}
          </h3>
        </header>

        {census === undefined ? (
          <p className="setup-note" role="status">
            {t("setup.loading")}
          </p>
        ) : census === null ? (
          // Not an empty table: a census that could not be taken and a mailbox
          // with no folders are different facts, and only one is a reason to go on.
          <div className="setup-body">
            <p className="banner banner-warn" role="alert">
              {t("setup.failed")}
            </p>
            <div className="setup-footer">
              <button
                type="button"
                className="button button-quiet"
                onClick={() => setReloads((n) => n + 1)}
              >
                {t("setup.reload")}
              </button>
              <button type="button" className="link-button" onClick={() => void finish()}>
                {t("setup.skip")}
              </button>
            </div>
          </div>
        ) : importMailboxId && !importFolder ? (
          <div className="setup-body">
            <p className="banner banner-warn" role="alert">
              {t("setup.import.missing")}
            </p>
            <div className="setup-footer">
              <button
                type="button"
                className="button button-quiet"
                onClick={() => navigate({ page: "settings", section: "areas" })}
              >
                {t("setup.import.done")}
              </button>
            </div>
          </div>
        ) : (
          <SetupBody step={step} goTo={goTo} shared={shared} />
        )}
      </section>
    </main>
  );
}

/**
 * WHICH STEP IS ON SCREEN.
 *
 * Its own component so the frame above stays about the census and the state: this
 * is a table from a number to a surface, and the Back/Next wiring that goes with
 * each one.
 */
function SetupBody({
  step,
  goTo,
  shared,
}: {
  step: SetupStep;
  goTo: (n: SetupStep) => void;
  shared: Omit<StepProps, "onNext" | "onBack">;
}): JSX.Element {
  switch (step) {
    case 4:
      // No `onNext`: step 4's way on is "Open the desk", which is `onFinish` and
      // leaves the wizard rather than advancing inside it.
      return <SetupDialsStep {...shared} onNext={() => undefined} onBack={() => goTo(3)} />;
    case 3:
      return <SetupRulesStep {...shared} onNext={() => goTo(4)} onBack={() => goTo(2)} />;
    case 2:
      return (
        <SetupMailStep
          {...shared}
          onNext={() => goTo(3)}
          // Import mode has no step 1 to go back to: it was opened from Settings,
          // and its own Done — and Escape — return there instead.
          onBack={shared.importFolder ? null : () => goTo(1)}
        />
      );
    default:
      return <SetupCensusStep {...shared} onNext={() => goTo(2)} onBack={null} />;
  }
}

/**
 * The row of buttons every step ends with: the way out, the way back, the way on.
 *
 * ONE COMPONENT because the ORDER is part of the design — leaving, then back,
 * then the primary action last — and three copies of that order would be three
 * chances for one step to disagree. Back is RENDERED AND DISABLED on step 1
 * rather than absent, so the footer does not reflow as the wizard advances and so
 * the control is announced consistently.
 */
export function SetupFooter({
  onBack,
  onSkip,
  next,
}: {
  onBack: (() => void) | null;
  /** Omitted in import mode: there is no wizard to skip, only one folder to
   * finish. */
  onSkip: (() => void) | null;
  next: { label: string; onClick: () => void; disabled?: boolean } | null;
}): JSX.Element {
  return (
    <div className="setup-footer">
      {onSkip ? (
        <button type="button" className="link-button" onClick={onSkip}>
          {t("setup.skip")}
        </button>
      ) : null}
      <button type="button" className="button button-quiet" disabled={!onBack} onClick={onBack ?? undefined}>
        {t("setup.back")}
      </button>
      {next ? (
        <button
          type="button"
          className="button button-primary"
          disabled={next.disabled ?? false}
          onClick={next.onClick}
        >
          {next.label}
        </button>
      ) : null}
    </div>
  );
}

/** A step's own region, so each one is a landmark with a name rather than a run
 * of divs. `children` is the step's controls; the heading is the frame's. */
export function StepBody({ children }: { children: ReactNode }): JSX.Element {
  return <div className="setup-body">{children}</div>;
}
