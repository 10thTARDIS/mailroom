// SPDX-License-Identifier: AGPL-3.0-only

// The editor is a separate chunk, fetched when the composer opens and never for
// the reading views (ADR-051 §6): about 150 KB gzip of editor code would otherwise
// sit in the main bundle for everyone, all the time. This file is the only thing
// the composer imports statically; the editor itself arrives through `import()`.

import { Component, Suspense, lazy } from "react";
import type { JSX, ReactNode } from "react";
import { t } from "../../i18n";
import type { RichTextEditorProps } from "./RichTextEditor";

const RichTextEditor = lazy(() => import("./RichTextEditor"));

/** While the chunk loads: the same box the editor will fill (so the layout does
 * not jump), announced politely. */
function Loading(): JSX.Element {
  return (
    <div className="editor editor-loading" role="status" aria-busy="true">
      {t("compose.editorLoading")}
    </div>
  );
}

/** If the chunk cannot be fetched (offline, a stale deploy), say so in place of
 * the editor instead of taking the whole composer down. The plain-text toggle
 * above it still works. */
class LoadFailure extends Component<{ children: ReactNode }, { failed: boolean }> {
  override state = { failed: false };
  static getDerivedStateFromError(): { failed: boolean } {
    return { failed: true };
  }
  override render(): ReactNode {
    if (this.state.failed) {
      return (
        <div className="editor editor-loading" role="alert">
          {t("compose.editorFailed")}
        </div>
      );
    }
    return this.props.children;
  }
}

export function RichEditor(props: RichTextEditorProps): JSX.Element {
  return (
    <LoadFailure>
      <Suspense fallback={<Loading />}>
        <RichTextEditor {...props} />
      </Suspense>
    </LoadFailure>
  );
}
