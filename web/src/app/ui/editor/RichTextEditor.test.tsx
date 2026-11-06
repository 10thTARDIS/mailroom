// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The compose editor's behaviour (ADR-051 beta 1): the features the execCommand
// editor had, through Tiptap. Each toolbar action is tested twice, once by the
// button and once by its keyboard shortcut, because the shortcut is a separate
// code path (a ProseMirror keymap) that a button test never touches.

import { afterEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { button, click, mountEditor, paste, press } from "./editorTestkit";
import type { Mounted } from "./editorTestkit";

let m: Mounted | null = null;
afterEach(async () => {
  vi.restoreAllMocks();
  await m?.unmount();
  m = null;
});

async function selectAll(mm: Mounted): Promise<void> {
  await act(async () => {
    mm.editor.commands.selectAll();
  });
}

describe("toolbar buttons", () => {
  const cases: Array<[string, string]> = [
    ["Bold", "<strong>hello</strong>"],
    ["Italic", "<em>hello</em>"],
    ["Underline", "<u>hello</u>"],
    ["Bulleted list", "<ul><li><p>hello</p></li></ul>"],
    ["Numbered list", "<ol><li><p>hello</p></li></ol>"],
  ];
  for (const [name, want] of cases) {
    it(`${name} formats the selection`, async () => {
      m = await mountEditor("<p>hello</p>");
      await selectAll(m);
      await click(button(m, name));
      expect(m.html()).toContain(want);
      expect(m.changes.at(-1)).toContain(want); // and the draft is told
    });
  }

  it("pressed state follows the selection and toggles off again", async () => {
    m = await mountEditor("<p>hello</p>");
    await selectAll(m);
    expect(button(m, "Bold").getAttribute("aria-pressed")).toBe("false");
    await click(button(m, "Bold"));
    expect(button(m, "Bold").getAttribute("aria-pressed")).toBe("true");
    await click(button(m, "Bold"));
    expect(m.html()).toBe("<p>hello</p>");
  });
});

describe("keyboard shortcuts", () => {
  const cases: Array<[string, { key: string; keyCode: number; shiftKey?: boolean }, string]> = [
    ["Ctrl+B", { key: "b", keyCode: 66 }, "<strong>hello</strong>"],
    ["Ctrl+I", { key: "i", keyCode: 73 }, "<em>hello</em>"],
    ["Ctrl+U", { key: "u", keyCode: 85 }, "<u>hello</u>"],
    ["Ctrl+Shift+8", { key: "8", keyCode: 56, shiftKey: true }, "<ul>"],
    ["Ctrl+Shift+7", { key: "7", keyCode: 55, shiftKey: true }, "<ol>"],
  ];
  for (const [name, key, want] of cases) {
    it(`${name} formats the selection`, async () => {
      m = await mountEditor("<p>hello</p>");
      await selectAll(m);
      await press(m, { ...key, ctrlKey: true });
      expect(m.html()).toContain(want);
    });
  }

  it("the button titles list the shortcuts", async () => {
    m = await mountEditor("");
    expect(button(m, "Bold").title).toBe("Bold (Ctrl+B)");
    expect(button(m, "Italic").title).toBe("Italic (Ctrl+I)");
    expect(button(m, "Underline").title).toBe("Underline (Ctrl+U)");
    expect(button(m, "Bulleted list").title).toBe("Bulleted list (Ctrl+Shift+8)");
    expect(button(m, "Numbered list").title).toBe("Numbered list (Ctrl+Shift+7)");
  });

  it("Ctrl+Enter is left for the composer to send, and adds no line break", async () => {
    m = await mountEditor("<p>hello</p>");
    const before = m.html();
    await press(m, { key: "Enter", keyCode: 13, ctrlKey: true });
    expect(m.html()).toBe(before);
  });

  it("control: Shift+Enter still makes a line break (the Ctrl+Enter test can fail)", async () => {
    m = await mountEditor("<p>hello</p>");
    await press(m, { key: "Enter", keyCode: 13, shiftKey: true });
    expect(m.html()).toContain("<br>");
  });

  it("Tab leaves the editor instead of indenting a list item", async () => {
    m = await mountEditor("<ul><li><p>one</p></li><li><p>two</p></li></ul>");
    await act(async () => {
      m!.editor.commands.focus("end");
    });
    const ev = await press(m, { key: "Tab", keyCode: 9 });
    expect(ev.defaultPrevented).toBe(false);
    expect(m.html()).not.toContain("<ul><li><p>one</p><ul>");
    expect(m.html()).toBe("<ul><li><p>one</p></li><li><p>two</p></li></ul>");
  });
});

describe("the toolbar as a widget", () => {
  it("is a named toolbar with one tab stop, moved by the arrow keys", async () => {
    m = await mountEditor("");
    const bar = m.host.querySelector('[role="toolbar"]') as HTMLElement;
    expect(bar.getAttribute("aria-label")).toBe("Text formatting");
    const tools = Array.from(bar.querySelectorAll<HTMLButtonElement>("button"));
    expect(tools).toHaveLength(7);
    expect(tools.map((b) => b.tabIndex)).toEqual([0, -1, -1, -1, -1, -1, -1]);
    tools[0]!.focus();
    await act(async () => {
      tools[0]!.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowRight", bubbles: true, cancelable: true }));
    });
    expect(tools.map((b) => b.tabIndex)).toEqual([-1, 0, -1, -1, -1, -1, -1]);
    expect(document.activeElement).toBe(tools[1]);
    await act(async () => {
      tools[1]!.dispatchEvent(new KeyboardEvent("keydown", { key: "End", bubbles: true, cancelable: true }));
    });
    expect(document.activeElement).toBe(tools[6]);
    await act(async () => {
      tools[6]!.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowRight", bubbles: true, cancelable: true }));
    });
    expect(document.activeElement).toBe(tools[0]); // wraps
  });

  it("the editing surface is a labelled multi-line textbox", async () => {
    m = await mountEditor("");
    expect(m.surface.getAttribute("role")).toBe("textbox");
    expect(m.surface.getAttribute("aria-multiline")).toBe("true");
    expect(m.surface.getAttribute("aria-label")).toBe("Message");
    expect(m.surface.getAttribute("contenteditable")).toBe("true");
  });

  it("injects no stylesheet of its own", async () => {
    const before = document.querySelectorAll("style").length;
    m = await mountEditor("");
    expect(document.querySelectorAll("style").length).toBe(before);
    expect(document.querySelector("style[data-tiptap-style]")).toBeNull();
  });
});

describe("links", () => {
  async function addLink(answer: string | null): Promise<Mounted> {
    const mm = await mountEditor("<p>hello</p>");
    vi.spyOn(window, "prompt").mockReturnValue(answer);
    await selectAll(mm);
    await click(button(mm, "Insert link"));
    return mm;
  }

  it("an https address becomes a bare link", async () => {
    m = await addLink("https://example.com/a");
    expect(m.html()).toBe('<p><a href="https://example.com/a">hello</a></p>');
  });

  it("a mailto address is allowed", async () => {
    m = await addLink("mailto:you@example.com");
    expect(m.html()).toContain('<a href="mailto:you@example.com">');
  });

  it("a bare address gets https:// in front", async () => {
    m = await addLink("www.example.com");
    expect(m.html()).toContain('<a href="https://www.example.com">');
  });

  for (const bad of [
    "javascript:alert(1)",
    "JaVaScRiPt:alert(1)",
    "java\tscript:alert(1)",
    " javascript:alert(1)",
    "data:text/html,<b>x</b>",
    "vbscript:x",
    "file:///etc/passwd",
    "ftp://example.com/x",
    "tel:+15555550100",
  ]) {
    it(`refuses ${JSON.stringify(bad)} and says so`, async () => {
      m = await addLink(bad);
      expect(m.html()).toBe("<p>hello</p>");
      expect(m.notices).toEqual(["Only web (http, https) and mail (mailto) addresses can be used as links."]);
    });
  }

  it("cancelling the prompt changes nothing and says nothing", async () => {
    m = await addLink(null);
    expect(m.html()).toBe("<p>hello</p>");
    expect(m.notices).toEqual([]);
  });

  it("typing a bare URL does not turn it into a link (no autolink, as before)", async () => {
    m = await mountEditor("<p>see https://example.com now</p>");
    expect(m.html()).not.toContain("<a");
  });
});

describe("pictures", () => {
  it("the picture button's file goes through the upload and lands as a cid: image", async () => {
    m = await mountEditor("<p>hello</p>");
    const input = m.host.querySelector('input[type="file"]') as HTMLInputElement;
    const file = new File(["x"], "cat.png", { type: "image/png" });
    Object.defineProperty(input, "files", { value: [file], configurable: true });
    await act(async () => {
      input.dispatchEvent(new Event("change", { bubbles: true }));
    });
    expect(m.uploads).toEqual([file]);
    // The serializer normalises the style declaration's spacing; same value.
    expect(m.html()).toContain('<img src="cid:img-1@inline.mailroom" alt="cat.png" style="max-width: 100%;">');
  });

  it("a pasted image FILE (a screenshot) is uploaded and becomes a cid: image", async () => {
    m = await mountEditor("<p>hello</p>");
    const file = new File(["x"], "shot.png", { type: "image/png" });
    await paste(m, { files: [file] });
    expect(m.uploads).toEqual([file]);
    expect(m.html()).toContain('src="cid:img-1@inline.mailroom"');
  });

  it("a failed upload inserts nothing", async () => {
    m = await mountEditor("<p>hello</p>", { onImageFile: async () => null });
    await paste(m, { files: [new File(["x"], "shot.png", { type: "image/png" })] });
    expect(m.html()).toBe("<p>hello</p>");
  });

  it("a non-image file is not taken", async () => {
    m = await mountEditor("<p>hello</p>");
    await paste(m, { files: [new File(["x"], "a.pdf", { type: "application/pdf" })] });
    expect(m.uploads).toEqual([]);
  });

  it("a copy from a web page (file plus markup naming the remote original) is not uploaded; the markup path owns it", async () => {
    m = await mountEditor("<p>hello</p>");
    await paste(m, {
      files: [new File(["x"], "pic.png", { type: "image/png" })],
      html: '<img src="https://images.example.com/pic.png">',
    });
    expect(m.uploads).toEqual([]);
    expect(m.html()).not.toContain("images.example.com");
  });
});

describe("seeding and reading back", () => {
  const quote =
    "<p><br></p><p>On Monday, Ann wrote:</p><blockquote>line one &lt;b&gt;not bold&lt;/b&gt;<br>line two &amp; three</blockquote>";

  it("a reply quote stays escaped plain text inside a blockquote", async () => {
    m = await mountEditor(quote);
    const bq = m.surface.querySelector("blockquote") as HTMLElement;
    expect(bq).not.toBeNull();
    expect(bq.textContent).toBe("line one <b>not bold</b>line two & three");
    expect(bq.querySelector("b")).toBeNull(); // the angle brackets stayed text
    expect(m.html()).toContain("<blockquote><p>line one &lt;b&gt;not bold&lt;/b&gt;<br>line two &amp; three</p></blockquote>");
  });

  it("getHTML round-trips the seeded content", async () => {
    const seed = "<p>one</p><p><strong>two</strong> and <em>three</em></p><ul><li><p>a</p></li><li><p>b</p></li></ul>";
    m = await mountEditor(seed);
    expect(m.html()).toBe(seed);
  });

  it("an empty paragraph is written with a break, so recipients keep the blank line", async () => {
    m = await mountEditor(quote);
    expect(m.html().startsWith("<p><br></p><p>On Monday")).toBe(true);
  });

  it("seeding does not call onChange (an untouched body stays untouched)", async () => {
    m = await mountEditor(quote);
    expect(m.changes).toEqual([]);
  });

  it("an edit calls onChange with the new HTML", async () => {
    m = await mountEditor("<p>hello</p>");
    await act(async () => {
      m!.editor.commands.insertContent(" world");
    });
    expect(m.changes.at(-1)).toContain("world");
  });
});

describe("paste as plain text (Ctrl+Shift+V)", () => {
  const data = { html: "<p><strong>bold</strong> words</p>", text: "bold words" };

  it("a plain paste keeps the formatting", async () => {
    m = await mountEditor("");
    await paste(m, data);
    expect(m.html()).toContain("<strong>bold</strong>");
  });

  it("a paste after Ctrl+Shift+V is plain text", async () => {
    m = await mountEditor("");
    await press(m, { key: "V", keyCode: 86, ctrlKey: true, shiftKey: true });
    await paste(m, data);
    expect(m.html()).toBe("<p>bold words</p>");
  });
});
