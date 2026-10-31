// SPDX-License-Identifier: AGPL-3.0-only

// CONNECT AN AI TOOL (#49, ADR-044). Shown twice: on the API keys row with a
// placeholder where the key goes, and inside the token dialog with the new key
// filled in — the one moment a person has the key and the instructions in the
// same place.
//
// GENERIC FIRST: the URL (this page's origin plus /mcp), the header, and the
// sentence that any MCP client with Streamable HTTP and headers works. Then the
// generic stdio bridge, then two worked examples, labelled as examples. The
// words and snippets are docs/mcp.md's (setup.ts; setupDocs.test.ts holds them
// together).

import { useId, useRef, useState } from "react";
import type { JSX } from "react";

import { t } from "../../../i18n";
import { authorizationHeader, bridgeSnippet, mcpEndpoint, needsAllowHttp, remoteYamlSnippet, toolsFor } from "./setup";
import { PRESETS, presetName } from "./model";

/** Copy text with a hidden textarea and execCommand, for origins where
 * navigator.clipboard does not exist (anything but https or localhost: the test
 * instance is plain http on a LAN). The textarea goes INSIDE `wrapper`, never
 * document.body: the token dialog is modal and everything outside it is inert,
 * and an inert element cannot take focus or a selection. Returns whether the
 * browser said it copied. */
function copyWithSelection(wrapper: HTMLElement, text: string): boolean {
  const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.readOnly = true;
  ta.setAttribute("aria-hidden", "true");
  ta.tabIndex = -1;
  ta.style.position = "fixed";
  ta.style.top = "0";
  ta.style.left = "-9999px";
  ta.style.opacity = "0";
  wrapper.appendChild(ta);
  try {
    ta.focus();
    ta.select();
    ta.setSelectionRange(0, text.length);
    return document.execCommand("copy");
  } catch {
    return false;
  } finally {
    ta.remove();
    previous?.focus();
  }
}

/** Copy text to the clipboard: the async Clipboard API where the browser has it
 * (a secure context), else, or if it refuses, a selection copy. Where both fail
 * the snippet beside the button is still selectable, and the status line says
 * to copy it by hand rather than claiming it was copied. */
export function CopyButton({ text, label }: { text: string; label: string }): JSX.Element {
  const [state, setState] = useState<"idle" | "copied" | "failed">("idle");
  const wrapper = useRef<HTMLSpanElement>(null);
  const copy = async (): Promise<void> => {
    if (navigator.clipboard) {
      try {
        await navigator.clipboard.writeText(text);
        setState("copied");
        return;
      } catch {
        // Refused (no permission, or a frame without it): try the selection copy.
      }
    }
    setState(wrapper.current && copyWithSelection(wrapper.current, text) ? "copied" : "failed");
  };
  return (
    <span className="copy-control" ref={wrapper}>
      <button type="button" className="button" onClick={() => void copy()}>
        {label}
      </button>
      <span className="field-hint" role="status">
        {state === "copied" ? t("apiKeys.copy.done") : state === "failed" ? t("apiKeys.copy.failed") : ""}
      </span>
    </span>
  );
}

function Snippet({ text, label, copyLabel }: { text: string; label: string; copyLabel: string }): JSX.Element {
  const id = useId();
  return (
    <div className="setup-snippet">
      <p className="field-label" id={id}>
        {label}
      </p>
      <pre className="setup-code" aria-labelledby={id} tabIndex={0} data-scroll-x="intentional">
        <code>{text}</code>
      </pre>
      <CopyButton text={text} label={copyLabel} />
    </div>
  );
}

export function SetupGuide({
  origin,
  token,
  scopes,
  restricted = false,
  subheading = "h5",
}: {
  origin: string;
  /** The real key (token dialog), or null for the placeholder. */
  token: string | null;
  /** The key's scopes, to list its tools; null lists the tools per preset. */
  scopes: readonly string[] | null;
  restricted?: boolean;
  /** The level of this guide's own subheadings, one below wherever it sits. */
  subheading?: "h4" | "h5";
}): JSX.Element {
  const H = subheading;
  const endpoint = mcpEndpoint(origin);
  const key = token ?? t("apiKeys.setup.placeholder");
  return (
    <div className="setup-guide">
      <p>{t("apiKeys.setup.intro")}</p>
      <dl className="setup-generic">
        <dt>{t("apiKeys.setup.url")}</dt>
        <dd>
          <code className="setup-inline">{endpoint}</code>
        </dd>
        <dt>{t("apiKeys.setup.transport")}</dt>
        <dd>{t("apiKeys.setup.transportValue")}</dd>
        <dt>{t("apiKeys.setup.header")}</dt>
        <dd>
          <code className="setup-inline">{authorizationHeader(key)}</code>
        </dd>
      </dl>
      <p className="field-hint">{t("apiKeys.setup.anyClient")}</p>
      <p className="field-hint">{t("apiKeys.setup.keepSecret")}</p>
      {needsAllowHttp(endpoint) ? (
        <p className="banner banner-warn" role="note">
          {t("apiKeys.setup.plainHttp")}
        </p>
      ) : null}

      <H>{t("apiKeys.setup.bridgeHeading")}</H>
      <p>{t("apiKeys.setup.bridgeIntro")}</p>
      <Snippet text={bridgeSnippet(endpoint, key)} label={t("apiKeys.setup.bridgeLabel")} copyLabel={t("apiKeys.setup.copyConfig")} />
      <p className="field-hint">{t("apiKeys.setup.bridgeSpace")}</p>
      <p className="field-hint">{t("apiKeys.setup.bridgeProcessList")}</p>

      <H>{t("apiKeys.setup.examplesHeading")}</H>
      <p className="field-hint">{t("apiKeys.setup.examplesNote")}</p>
      <p>{t("apiKeys.setup.desktopExample")}</p>
      <p>{t("apiKeys.setup.localExample")}</p>
      <Snippet
        text={remoteYamlSnippet(endpoint, key)}
        label={t("apiKeys.setup.localLabel")}
        copyLabel={t("apiKeys.setup.copyConfig")}
      />
      <p className="field-hint">{t("apiKeys.setup.localNote")}</p>

      <H>{t("apiKeys.setup.toolsHeading")}</H>
      {scopes ? (
        <ToolLine label={t("apiKeys.setup.toolsThisKey")} tools={toolsFor(scopes, restricted)} />
      ) : (
        <>
          {PRESETS.map((p) => (
            <ToolLine key={p.id} label={presetName(p.id)} tools={toolsFor(p.scopes, false)} />
          ))}
          <ToolLine label={t("apiKeys.setup.toolsSend")} tools={toolsFor(["mail:send"], false).filter((x) => x !== "get_pending")} />
          <ToolLine label={t("apiKeys.setup.toolsDelete")} tools={toolsFor(["mail:delete"], false).filter((x) => x !== "get_pending")} />
        </>
      )}
      <p className="field-hint">{t("apiKeys.setup.toolsNote")}</p>
    </div>
  );
}

function ToolLine({ label, tools }: { label: string; tools: readonly string[] }): JSX.Element {
  return (
    <p className="setup-tools">
      <span className="setup-tools-label">{label}:</span>{" "}
      {tools.map((name, i) => (
        <span key={name}>
          {i > 0 ? ", " : null}
          <code>{name}</code>
        </span>
      ))}
    </p>
  );
}
