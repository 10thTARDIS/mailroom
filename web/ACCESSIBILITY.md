# Accessibility — the model this client is built on

Spec §12 lists screen-reader support, keyboard operation and contrast as
requirements. This file records **how** they are met, because most of the answer
is a small number of decisions that are invisible in a diff and easy to undo by
accident. It is the client's half of the accessibility work; the automated checks that
assert it are a separate increment (see the last section).

Everything here is observable in the DOM or in the accessibility tree. Nothing
below is a promise about intent.

## The message list is a grid, and focus lives on the grid

`ui/desk/VirtualList.tsx` renders the desk's list and the in-tray page's list as
a `role="grid"`:

| element | role | notes |
|---|---|---|
| the list (`ul.desk-list`) | `grid` | `tabIndex=0`, `aria-rowcount`, `aria-multiselectable`, `aria-activedescendant` |
| each item (`li.desk-item`) | `row` | `aria-rowindex` (one-based, into the whole list — not into what is rendered) |
| a message row's three regions | `gridcell` | sender · subject+preview · time and marks |
| an area heading | `rowheader` | holds the `<h2><button aria-expanded>` it already was |
| the line, a drawer, "show more", a section error, the empty state | one `gridcell` | |

**Focus is on the container, with `aria-activedescendant` naming the cursor row —
not a roving `tabIndex`.** The list is virtualised, so a row can unmount while it
holds focus; focus would fall to `<body>`, every key would go quiet, and nothing
on screen would say why. The container is never unmounted by scrolling, so the
only thing that can go stale is an attribute.

That trade has one obligation: **the row `aria-activedescendant` names must be in
the document.** An id resolving to nothing is worse than no attribute — the grid
claims a current row and a screen reader finds none. So the cursor row is
rendered even when the viewport has been scrolled away from it, at its true
offset (`pinnedWindow`, in `core/desk.ts` beside the window arithmetic it
extends, tested there).

The rows' own buttons are `tabIndex={-1}`: the keyboard acts on the cursor row,
and two tab stops per row meant Tab took forty presses to cross a screenful.
A click still focuses them, and the grid takes movement keys from there too.

### A row's name is a sentence

`ui/desk/rowName.ts` builds it, in this order, dropping every clause whose fact
is absent:

> **Unread**, sender, subject, `in <area>`, `<account> account`, `arrived at
> <alias>`, time, `In the tray`, `Pinned`, `Came back from a snooze`, `Has an
> attachment`, `An attachment is held`, the sender-warning sentence, `<n>
> messages in this conversation`, `waiting to send`, `Change not yet confirmed by
> the server`

Rules that are easy to break later:

- **Unread is a word.** On screen it is weight plus a dot; neither is audible.
  "Read" is deliberately never said — the absence is the statement.
- **Nothing is carried by colour alone.** The account dot, the warning glyph and
  the marks are `role="img"` with labels, and each is also a clause above. The
  row the reading pane is showing carries `aria-current` (an accent bar is not a
  word).
- **Two facts are legitimately absent, not broken.** `senderWarnings` and
  `attachmentScan` are body properties: a row fetched as part of a list does not
  have them, and the mark is not claimed. The in-tray page states no area,
  because its query never asked for one.
- The row is the announcement; the cell buttons are secondary. The sender button
  is "Select mail from …", the main button is "Open …" — it used to repeat the
  sender.

### The grid's keyboard

While focus is in the list:

| key | |
|---|---|
| `↑` / `↓`, `j` / `k` | previous / next message (`j`/`k` work from anywhere on the surface) |
| `Home` / `End` | first / last message |
| `PageUp` / `PageDown` | a screenful of rows |
| `Enter`, `o` | open |
| `Space` (`Shift+Space`) | select (extend the range) — `x` still does this too |
| `Shift+↑` / `Shift+↓` | put the message on the other side of the line |

`gridKeyIntent` (`ui/desk/gridKeys.ts`, pure and tested) decides what the grid
claims. It claims **nothing** carrying Ctrl/⌘/Alt, and nothing for `j`/`k` or
Shift+Arrow — those belong to the route's own chain, and the grid calls the
route's cursor mover rather than growing a second one, because with a message
open moving the cursor also moves the reading pane.

**`Tab` is Tab again.** It used to switch desk halves while focus was in the
list, which trapped the keyboard in both directions *and* meant Tab could never
reach the controls inside the list — a section's fold, a drawer's Open, the
line's stricter/looser were unreachable. The halves move on the tabs' own arrows.

## One live region

`ui/desk/HeaderSentence.tsx` is `role="status" aria-live="polite"`, and it is the
only live region on the desk. It already contains every number the rest of the
surface would have announced, so a second region is duplication that interrupts.
Section errors are deliberately **not** `role="alert"`.

The exceptions, all narrow: the shell's offline/error banners, the shell's
pending-approvals banner (an always-present `role="status"` wrapper,
empty while nothing is waiting, so the first request IS announced), and the
selection toolbar's count (`role="status"` — "2 selected", "3 selected"). The
count has a known limit written down beside it: a live region must exist before
its text changes, and the toolbar does not exist until something is selected, so
the *first* selection may not be announced.

Toasts are a `role="status" aria-live="polite"` container; an undo toast's
countdown is `role="timer"` (implicit `aria-live="off"`, so the number ticking
does not re-announce the sentence around it).

## Roles per surface

| surface | |
|---|---|
| rail / bottom bar | `<nav aria-label>` |
| the route | `<main id="messages" tabIndex={-1}>` — the skip link's target, with a visible focus edge |
| reading pane | `<aside aria-label={subject}>` → `complementary` |
| desk halves | `role="tablist"` of `role="tab"`, one tab stop, `aria-controls` → the grid. Each tab is named explicitly ("Review first, 6 messages") rather than from its label plus a count pill, because the grid BORROWS that name: a grid cannot also be a `tabpanel`, so it takes the panel's place and its name |
| the line | `role="slider"`, `aria-valuemin=0` / `max=1` / `now`, and an `aria-valuetext` that states the split rather than the number |
| toolbars | `role="toolbar" aria-label`, every button a word |
| sender popover | `role="tooltip"` while hovered (the anchor points `aria-describedby` at it), `role="dialog"` once pinned |
| attachment viewer | `role="dialog" aria-modal`, focus trapped, Escape closes and returns focus |
| settings | `role="dialog" aria-modal`, focus trapped, with an `<h2>` for the page and an `<h3>` per row. Deliberately **not** a `main` landmark: the list route stays mounted underneath, so a second `main` would be a real violation |
| search box | the input is an ARIA 1.2 `combobox` that owns its `listbox` of suggestions through `aria-controls`/`aria-activedescendant`; focus never leaves the input, nothing is active until an arrow key, Escape collapses the list and keeps the text |
| search options | an in-page `<form>` named by its `<h2>`, opened by a button with `aria-expanded`/`aria-controls`; not a dialog and not trapped (the results it refines stay usable). Focus goes to its first field on open and back to the button on Escape or Cancel; every control is named by its visible `<label>`; one column at phone width |
| search sort | `role="group"` named "Sort search results" of two buttons with `aria-pressed` |
| key dialogs (Settings › Keys) | `role="dialog" aria-modal`, named by their `<h2>` (`aria-labelledby`), focus trapped, Escape closes and returns focus to the button that opened them. They open INSIDE the settings dialog, so each stops Tab and Escape from propagating: the page's own trap and the shell's Escape never see a key the dialog handled |
| revoke key | the confirm button is `aria-disabled`, NOT `disabled`, until a reason is chosen and the fingerprint's last 8 characters are typed: it stays a tab stop, is announced as unavailable, and its `aria-describedby` names what it is waiting for. A click on it while unavailable does nothing |
| key-expiry banner | a `banner-warn` block on the desk whose sentence is `role="status"`; "Extend it" and "Dismiss" are word buttons, Dismiss named for the key it dismisses |
| pending-approvals banner | in the shell, over every mail view: a `role="status"` (polite) wrapper that is always rendered and empty while nothing waits; "N actions waiting for your approval" and a word button, "Review", that opens Settings → API keys |
| API key token dialog | a labelled modal (`aria-labelledby` → its heading) that traps Tab and starts on the token field; it has NO close button and ignores Escape and clicks outside, because closing it is when the key is gone for good: only "Done, I've saved the key" closes it. Escape is still stopped there, so it cannot peel the settings page behind |
| approve request | Approve… opens a labelled modal that traps Tab, starts on Cancel, and states in one sentence exactly what approving does; the request's recipients, subject and names are each their own `<bdi dir="auto">`, never loose text, so right-to-left characters in them cannot reorder the page's own words; the server's answer is a `role="status"` line on the card |
| create API key | the Create button is `aria-disabled`, not `disabled`, until the form is complete (and, for send plus delete, until the phrase is typed), described by the sentence saying what it waits for; a click while unavailable sends nothing |
| invitation card | a `<section aria-label>` → `region` named "Calendar invitation: <title>" (or "Cancelled event: …", "Reply to an invitation: …"); Accept / Maybe / Decline are a `role="group"` named "Respond to this invitation" of buttons whose `aria-pressed` is the answer the SERVER recorded, after a reload, not the click. The answer is announced by the undo toast, so the card adds no live region |

**The two row layouts share one DOM.** A row is `role="row"` with three
`gridcell`s — sender, subject, marks and time — whichever of `One line` and
`Multi-line` is chosen, and its accessible name is the same sentence from
`ui/desk/rowName.ts` either way. Only CSS grid placement differs: on one line the
three cells are three columns; on three lines the sender and the marks share the
first line and the subject and the snippet take the two below. Nothing appears or
disappears from the accessibility tree, no element changes role, and no mark loses
its label — which is why every assertion in this file holds in both, and why the
lane lints the multi-line desk as a surface of its own rather than taking that on
trust.

**The text size is a `role="slider"` with a word for a value.** `aria-valuemin=1`,
`aria-valuemax=5`, `aria-valuenow` the step, and `aria-valuetext` the word
("Smallest", "Small", "Default", "Large", "Largest") — "3 of 5" is not a size.
Arrow keys move one step and clamp at both ends; `Home`/`End` ARE implemented
here, unlike on the line's slider, because the ends of this range really are
"smallest" and "largest". The five words are also buttons with `aria-pressed`,
placed as SIBLINGS of the slider rather than inside it: `slider` supports no
focusable descendants.

**The line's arrows follow ARIA, not the row's layout**: `↑`/`→` = stricter,
because a stricter line is a higher `aria-valuenow`. `Enter` applies a pending
line, `Escape` cancels. `Home`/`End` are deliberately unimplemented — their
slider meaning is "jump to the minimum/maximum", and the ends of this range are
"show everything" and "show nothing".

**A drawer's disclosure button is named** "Promotions drawer, 24 messages,
auto-archives after 30 days" (or "never auto-archives", or with ", sweep off"
appended). The visible word stays Open/Close; the state is `aria-expanded`'s job.

## Focus when a surface takes over

`ui/focusTrap.ts` is the one implementation. `tabTarget` — the wrap decision — is
pure and tested, including the case hand-rolled traps miss: focus on the dialog's
own container, where forward is already right and **backward walks out of the
back of the dialog**.

- **Trapped:** the attachment viewer, the desk's confirm dialog, settings (and
  the key dialogs inside it), and the composer when it has the main area or the
  phone screen. Each has something
  live behind it — the list beside it, or covered opaquely but still in the tab
  order. Settings opens on the search field and, on close, hands focus back to
  whatever opened it if that is still in the document, or to the desk's grid if
  not.
- **Not trapped:** focused reading. There the stylesheet gives the list
  `display: none`, so it is not in the tab order at all and there is nothing to
  trap focus away from; what *is* still on screen is the rail, the search box and
  the header, and trapping would cut the reader off from all three while claiming
  a modality the pane does not have. What it does instead is move focus into the
  pane when it takes over (an element that becomes `display: none` while focused
  hands focus to `<body>`) and hand it back to the grid when it closes.
- **Escape peels one layer, top-most first.** Each surface stops propagation —
  capture-phase on `window`, or React's `stopPropagation`, which halts the native
  event at the root container before the shell's window listener sees it. The
  menus and the account switcher are peeled in order inside the shell's own
  chain.

## Reaching a toast

`Ctrl/⌘+Z` runs the newest undo offer from wherever focus is. **`g u`** moves
focus to its Undo button instead, for a reader who wants to hear what happened
before deciding — and focusing it pauses the countdown (hover does too). Both are
in the keys sheet; neither is printed on the toast.

## Motion and colour

`prefers-reduced-motion: reduce` is answered once, in `styles.css`:
`* { transition: none !important; animation: none !important }`. `!important`
means stylesheet order cannot defeat it. There are two transitions in the whole
tree (the tray caret, the swipe snap-back), no `@keyframes`, no smooth scrolling,
and nothing calls `Element.animate` or `scrollIntoView`.

Colour is never the only carrier — see the row's name above, and `tokens.css`,
which declares its text/background pairs as machine-readable `@contrast`
comments that `scripts/check-tokens.mjs` checks in both themes as part of
`npm run build`. Focus is always a visible edge (a 2px inset outline).

## The phone

Same names, same roles. Both swipe actions have a named button equivalent that
needs no swipe: tapping the sender (or a long press anywhere on the row) selects
it, which docks the selection toolbar at the thumb with **Archive** and
**In-tray** as words. Nothing appears on hover over a row, on any width.

## What is CHECKED, and what is not

Everything above is asserted on every push by `scripts/a11y-e2e.sh` (CI job
`accessibility`), against the **real image** serving the **real built client**
over real HTTP, with a real Dovecot mailbox of thirteen messages behind it. The
desk it drives has two populated areas, a drawer with a count, an unread row, a
read row, a flagged row, an attachment and a two-message thread — and the lane
**fails if the fixture did not produce that**, because an assertion made against
an empty desk is the shape of the mistake this project keeps a rule about.

Two instruments, because one of them cannot see most of this file:

**axe-core** (exact-pinned devDependency; nothing in `src/` imports it) runs over
fifteen surfaces — the day-one setup wizard (`#/setup`), desk / Review first, desk
/ Everything else with a drawer open, the in-tray page, All mail, Settings,
Settings with a section open, the composer, **the composer reached by reloading its URL** (the warm path once stayed green while a reload hung), the reading pane, the sender popover
pinned, the attachment viewer, focused reading, **the desk in the multi-line row
layout**, and **the desk at the largest text size** — in **three themes** (light,
dark, and "system" with the OS asking for dark, which is a different code path).
The last two are chosen through the Appearance controls, like the theme, and put
back afterwards so the announcement script still runs on the default layout.
WCAG 2 A + AA violations fail the build, and every run prints its `passes`
denominator so `violations=0` cannot be mistaken for a linter that examined
nothing. `color-contrast-enhanced` (AAA, 7:1)
is measured separately and gated against `scripts/a11y-contrast.allow`, where each
line names an element, the **worst ratio it was actually measured at**, and why
4.5:1 is the right answer for it.

**Chromium's accessibility tree** — read through puppeteer's snapshot and CDP
`Accessibility.getPartialAXTree`, which is what a screen reader on that platform
consumes — is asserted after each step of the desk's core loop: opening the app,
moving between rows, archiving (with `g u`, the Undo button's name, and the
`role="timer"` holding while focused), flagging, crossing the line, the line's
slider and its `aria-valuetext`, switching halves and opening a drawer, and a
fault replacing the header sentence. Each step asserts the computed **role, name,
value and states** of the active row or focused node, plus the text of the live
region — the inputs to any announcement — and the transcript is printed, so the
sentence a reader would be given is in the log rather than behind a boolean.

**Four negative controls run in the same job and must each go red**: a nameless
button must make axe report `button-name`; a glyph-only button losing its
`aria-label` must make the tree check go red; `p.desk-header` losing `role` and
`aria-live` must fail the live-region check; `#desk-grid` losing
`aria-activedescendant` must fail the cursor check. A control that stays green
fails the run, so a green job also says the checks can fail.

The lane was probed the same way, by deleting `aria-rowindex` from
`ui/desk/VirtualList.tsx` and rebuilding the image: **all thirty-three axe passes
stayed green** and the lane went red on `NOACTIVE move: aria-rowindex went 0 -> 0`.
That is the argument for the second instrument, measured rather than asserted.

### The bounds, in words

- **No screen reader runs.** No runner here is Windows, so NVDA is unavailable and
  JAWS is not licensable in CI; VoiceOver has no automation surface. What is
  asserted is the tree an AT consumes, never an AT's output.
- **Nothing here is evidence about phrasing or verbosity.** Whether a comma is a
  pause, how much of a row is spoken at default verbosity, and whether a polite
  region is read exactly once are the AT's business. The `role="timer"` claim is
  checked structurally (its value holds while the toast is focused), not
  acoustically.
- **`aria-current` is read from the DOM, not the tree.** CDP's `AXPropertyName`
  has no `current`, so Chromium does not expose it as a computed property.
- **The message body is not linted.** It renders in a sandboxed, opaque-origin
  `srcdoc` iframe that axe cannot enter and must not — that document is a
  stranger's HTML, and `scripts/hostile-content-e2e.sh` is what governs it.
- **The attachment viewer's HELD panel is not covered.** It needs a quarantined
  part, which needs a real scanner detection (`scripts/clamav-test.sh`'s
  territory); the fixture cannot produce one cheaply. Named on the lane's final
  line rather than quietly skipped.
- **The phone form is not covered.** The lane runs at one desktop viewport, so
  the swipe equivalents and the bottom bar are asserted by this document and by
  the unit tests, not by the browser.

### What the lane found on its first complete run

Two things, both fixed here, and both worth reading as evidence about what these
instruments can see:

- `--fg-muted` on `--bg-selected` was **4.21:1 in dark — below AA** — on the
  rail's unread count for the mailbox you are in and on the attachment viewer's
  caption for the thumbnail you are on. `scripts/check-tokens.mjs` could not have
  found it: it checks the pairs `tokens.css` declares, and this pair existed only
  because two rules met in the cascade. The pair is now declared (so it is a red
  build in both themes) and the dark value moved to `#1d3450`.
- **`g u` lost the reader's place.** Escape out of the focused undo toast removed
  the focused element, so focus fell to `<body>` — the exact failure this document
  already warns about twice, in the key increment A added for screen-reader users.
  `ui/Toasts.tsx` now hands focus back to where `g u` took it from, or to the grid.

### The DOM ids the lane relies on

| | |
|---|---|
| the desk's grid | `#desk-grid` |
| the in-tray page's grid | `#tray-grid` |
| a row | `<gridId>-item-<itemKey>`, non-word characters replaced by `-` (`#desk-grid-item-r-01J…` for a message row, `#desk-grid-item-line`, `#desk-grid-item-d-<areaId>`) |
| the desk's tabs | `#desk-tab-review`, `#desk-tab-rest` |
| the line's slider | `#desk-line-slider` |
| the header sentence | `p.desk-header[role=status]` |
| the toast container | `div.toasts[role=status]`, an undo toast's button `.toast-undo .toast-undo-action`, its countdown `.toast-countdown[role=timer]` |
| the sender popover | `#sender-popover-<messageId>` |
| a settings row's body | `#setting-body-<rowId>` |
| the search box | `input.search-input`, its listbox `#search-suggestions`, options `#search-suggestions-<n>`; the options form `#search-options`, its fields `#search-options-<field>` |
