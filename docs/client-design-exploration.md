# Client design exploration: six directions to react to

*Forgejo #371, first issue of the v1.65.0 client experience rebuild. A seventh direction, written
from a reader's reaction to the first six, is at the end of the directions.*

This is a document to point at, not a plan. It offers six genuinely different answers to "what
should Mailroom's client be", each with its trade-offs named, so that a reader who cannot articulate
what they want in advance can still say "that one", "not that", or "the middle bit of that one and
the settings of this one". None of these is recommended over the others here. No implementation is
planned and nothing is estimated.

## How this was produced

One brief described what the server can do and what constrains the client: the capabilities that
exist today with only a thin surface (explainable priority with correction, function-derived
categories with abstention, induced rules, withheld attachments, key discovery, sender warnings,
send-later, the catch-all alias model), the constraints (self-hosted, one to a few users, web and PWA
on phone and desktop, keyboard-driven, dark and light as equals, no LLM tier), the unresolved
question (three inbox views that are three separate places today), the problems to carry (settings as
modal sheets, invisible views, no release interaction, no encrypted-mail surface, no home for account
*state*), and the whole backlog through the third major version, which each direction was asked to
make room for and to mark for carry-forward into the native and desktop clients.

The brief deliberately withheld the current screens, component names and stylesheets. Four models
across capability tiers (Haiku, Sonnet, Opus and Fable) each produced two directions independently:
one they believed in and one deliberately uncomfortable. Nothing was averaged. Where a cheaper model
and a frontier model converged, that is recorded below as the probable baseline; where they diverged,
that is where the real decisions are.

Two things were added after the fact. Every claim a direction made about the server was checked
against the code, and the corrections are in their own section. And two directions named a
measurement that would decide their fate against a real mailbox; those are described in the
"Measurements" section as questions to answer on your own mailbox, not as results.

## The question at the centre

There are three ways to look at an inbox in this product: by **priority** (a learned score, with
buckets and an explanation), by **arrival** (reverse-chronological), and by **category**
(function-derived tabs, with a needs-a-category queue for what the classifier declined to file). Today
they are three destinations, and nothing tells a new user they exist. The provisional proposal was
"let the user pick which one *is* the inbox". Every direction below rejects that framing, in six
different ways, which is the first finding.

---

## Family I: one list, and the score does something to it

Three directions keep a single unified list as the primary surface. They disagree, sharply, about
what the priority score is allowed to do to it.

### 1. Above the Line

*Frontier tier. The direction its author believed in.*

**Thesis.** The client is one list with one line drawn across it. Everything the model judged
important sits above the line; everything else below; the low bucket is folded away at the bottom.
Within each region the order is plain arrival. Category is not a place you go: it is a word on every
row and a filter one key away. Priority partitions the list, arrival orders it, category annotates it.
The relationship with the model is physical: a message on the wrong side of the line is keyed across
it, and that crossing is the correction the model learns from.

**Optimises for** a single place to look, legibility of the model's judgement, a correction gesture
that costs less than reading the message, and one list to traverse by keyboard. **Gives up** category
as a primary structure (no tabs; "show me all receipts" is a filter and the line persists inside it),
ordering by score (score order jitters as the model updates and hides what is new; the score is never
shown as a number, only position and reasons), "split inbox" as a future feature (the line is the
split), and any reduction in reading.

**First screen.** Wide: a narrow rail (accounts; inbox, queued sends, snoozed, archive; labels and
saved searches folded), the list, a reading pane. Phone: the list alone. The list header states its
own condition, account colour dots and "synced 40 s ago"; if any account is paused or failing, the
fault replaces that sentence in the warning tone, so an inbox quiet because sync stopped can never
look like one quiet because nothing arrived. Rows carry sender (with a warning mark for a colliding,
punycode or unauthenticated-own-domain sender), subject and snippet, the alias it arrived at in muted
type, the category as a short word, a thread count when more than one, and time. Snooze returns and
bounces appear as "returned" rows. Then the line: a hairline labelled "12 above, 41 below, 6 low",
with a handle; pressing it offers stricter or looser *with a preview in counts* ("one step stricter
moves 4 of these 12 below") and nothing changes until confirmed. When the needs-a-category queue is
non-empty, one row sits directly under the line: "14 messages need a category"; when empty, the row is
absent and the queue's health is reported in settings, never by a blank. Empty state: "Nothing above
the line. 41 below." Nothing ever reads "No mail" without the sync sentence beside it.

**The three views.** Partition, order, annotation; none is a separate place. The category filter is a
number key; the same number re-files the focused message. Correction is crossing the line: the row
moves at once, its "why" strip records "moved by you", and the model is told. The needs-a-category row
opens a decision mode, one message at a time, category words on the number keys, plus "make this a
rule for this sender" with the rules engine's preview shown before acceptance.

**Settings.** One full-height page, reached by one key and left by Escape to wherever you were: a
single searchable list where typing filters rows across every section. Sections are the nouns the
server has, and every section header shows *state*, not just controls ("Categories: 2,281 filed, 0
pending, last run 2 min ago"; "Accounts: sync state in words, lag, last error, quota headroom").
Identities is where dozens of aliases live without a picker, grouped active and dormant by recency;
in compose, From is a stated fact ("this alias, because you are replying to mail sent there") with an
override that autocompletes.

**Keyboard and phone.** The list holds focus; single letters are verbs on the focused row; numbers are
categories; a chord crosses the line; a leader key then a letter changes scope; slash searches;
question mark shows the map. Every verb's key is printed beside it, and the command palette lists the
same verbs with the same keys and so teaches them. Phone: the list is the screen, line and header
unchanged; two swipes for archive and snooze; long press opens the why strip with "move above" and
"move below" as buttons; the category filter becomes a scrolling row of words.

**The awkward things.** Attachments are a row of tiles in the reading pane; a held one is drawn
hollow with its name, a "held" label and the reason in words ("could not be scanned:
password-protected archive"). Opening it opens an explanation, not the file: what held means, that
release lets it be opened, saved and forwarded and covers every copy, and two buttons, Release and
Keep held; flagged files ask twice; after release the tile notes "released by you" and the date. The
reading header carries the seal state in words; an undecryptable message shows, where the body would
be, why (encrypted to a key you do not hold, with the fingerprint) and two actions (share your key
page; reply asking for a re-send); in compose each recipient carries a key mark and the send area
states "will encrypt" or "cannot encrypt: no key for x". Account state appears in four places that
display one fact: the header sentence, the rail's account dot (hollow when paused), the empty-state
sentence, and the Accounts section. A row is a thread; with one message it looks like a message row.
A long queue opens with a report before any decision ("400 pending; 38 senders account for 310")
and offers clusters with rule previews, then the long tail one at a time.

**Backlog.** Nudges are "returned" rows above the line, in the slot snooze returns already use.
Agent confirmations sit in the queued-sends scope beside held sends ("agent asks to send / delete:
approve or deny"). An LLM summary is a folded, labelled line at the top of an opened thread, absent
when not installed. A calendar invite is a card in the attachment row's position. A shared mailbox is
another account colour in rail and header.

**Carry-forward.** Inherited by native and desktop clients: the one-list-one-line architecture; the
bucket words (above, below, low); the why strip and the crossing as the correction; the category words
and their numbers; the header sentence as the home of account state; the held, released and seal
vocabularies; settings as a searchable list showing state per section; colour spent only on accounts
and labels, with semantic tones for warning, held, sealed and returned defined equally in both themes;
layering by tone steps and hairlines, never transparency. Web-only: the three-pane layout and its
resizing; the exact leader-key grammar (the desktop client inherits it, the phone does not); the
drag-across-the-line gesture.

**What would have to be true.** That after a modest number of corrections the set above the line is
small and stable: a dozen rows a day, not a hundred, and rows do not flip sides between visits. If the
model is noisy, the line becomes something the user stops believing, and the design degrades into a
plain list with a jittery divider.

**Server work.** None. The source assumed attachment release had no client-callable API; it does (see
corrections).

### 2. Intact Timeline

*Near-frontier tier. The direction its author believed in.*

**Thesis.** Mailroom is a time-ordered record of what arrived, annotated with everything the server
concluded about it. The client never reorders mail; it compresses, marks and explains, but the spine
is always arrival and always the same spine, so "I have read down to here" stays true. Priority is a
rendering weight, not a sort. Category is a fold label and a search facet, not a room. The second job,
equal to reading, is being auditable: every automatic act the server took is an entry in a machine
log with a cause and an undo. For people who distrust software that acts on their mail without saying
why, a predictable ordering plus a ledger of the machine's work beats a clever ranking.

**Optimises for** the property mail clients keep destroying, a stack you can pop; scale invariance
(one list, one key set, 2,300 messages or 200,000); and legibility of automation. **Gives up** the
signal-first experience (after a bad hour of noise you expand folds to reach the four messages that
mattered; if the model is excellent, this wastes it on purpose), category as a place, and ambition:
the score may only hide, never surface.

**First screen.** A single full-height list of the unified inbox, newest first, with day headers. Each
row: sender name; **the alias the message arrived to** as a small persistent chip ("the only
classification here the user declared themselves, one address per service, free and never wrong");
subject; snippet; time; at most one status mark (held attachment, failed authentication, verified
signature, queued reply). A narrow rail: unified inbox, one entry per account with its **state** dot,
saved searches, labels, and the machine log with an attention count. Above the list, a search field
that is also the filter expression: clicked chips become editable text, typed text becomes chips. On
desktop, a reading pane with a **record margin** beside it. Low-priority mail is not removed; it
collapses into an inline fold at its own chronological position ("9 low-priority, 06:12 to 09:40,
mostly receipts"), and folds never span a day boundary.

**The three views.** Arrival is the only ordering, permanently. Priority becomes the fold, with a
threshold adjustable transiently from the list header and durably in settings; set it to zero and
nothing folds, which is how you turn priority off, and it is honest because the score still appears in
each message's record. Category becomes the *name* of a fold (folds group by category inside a time
band) and a facet in the search bar. Ranked reading survives as a temporary re-sort that changes the
list header to announce it is not arrival order and reverts on navigation, never sticky. The
needs-a-category queue gets its own kind of fold with its own voice ("4 messages the classifier
declined to file"), never absorbed into a category fold, plus a rail entry with a count; assigning a
category inside the fold is two keys and can be done in a run.

**Settings.** A full-page destination, never sheets, with sections down its left edge and a search
field at the top that indexes settings **and their values**: typing an alias finds the identity that
uses it, typing a sender finds the rule that names it, because people remember the long tail by its
content, not its label. Ways in: the rail, that search, the palette, and a "configure" affordance in
the reading surface that deep-links to the right section with the item focused. Settings is a route,
so Escape and the browser's back agree, and return restores the exact list position.

**Keyboard and phone.** Modeless single keys for move, open, archive, star, pin, snooze, mute,
select, return, with the same keys acting on a selection for bulk; a prefix key navigates; slash
searches; the help overlay is generated from the same table that binds the keys so it cannot drift.
Two keys are specific here: one opens the **record** for the selected row (score and why, category
and why, rules that matched, label suggestions not applied, authentication results, key status,
scanner verdict, bounces); one opens the row's action menu, which offers exactly the palette's verbs.
Phone: the rail becomes four bottom destinations and the record a sheet dragged up from the message;
folds matter more, not less; swipes leave a bar that names what happened with an undo, in words.

**The awkward things.** A held file appears where a file would appear, named and sized, visibly held
("held: this file could not be scanned (password-protected archive)"); one control, Release, opens a
one-sentence dialogue on what is being trusted and that release covers this content everywhere it
appears, now and later; afterwards the file keeps a permanent mark naming who released it and when.
The language never accuses: "could not be scanned" is the common case, and calling it dangerous trains
people to click through. An undecryptable message is not an error page: the row stays in the
timeline, the body names the key it needs, says Mailroom does not hold it, and offers export the raw
message, look up the key, ask the sender; in compose the send control states what will happen
("signed, encrypted to two of three recipients") so a downgrade is never silent. Account state is a
dot on the account's rail entry with a word beside it whenever it is not green ("sync off",
"credentials rejected", "behind 4h"), a persistent non-dismissible line above the inbox naming the
account and how long it has been that way, and a machine-log entry with its cause. Conversations are
server-collapsed, one row with a count only when the count exceeds one. Four hundred needing a
category graduates from fold to full surface, pre-clustered by the axes that explain it (alias,
sending domain, rules that nearly fired), with per-cluster decisions that preview exactly which
messages are affected; its empty state is "nothing awaiting a decision, last decided three days ago",
never "all done".

**Backlog.** Nudges: a fold pinned at the top of today, split waiting-on-you and waiting-on-them.
Agent confirmations: a blocking entry kind in the machine log, already the place where "something
wants to act on your mail" is answered. LLM summary: a marked, clearly generated block in the record
margin, wholly absent when the tier is not installed. Calendar invite: an action strip above the body.
Shared mailbox: another rail entry with its own state dot, the alias chip carrying acting-as identity
into compose. Split inbox: deliberately no home; two simultaneous lists is the thing this direction
refuses, and saved searches pinned to the rail are the honest eighty per cent.

**Carry-forward.** Carried: the intact chronology; the **fold** as the single compression primitive
for everything the system de-emphasises; the alias chip as part of a row's identity; the **record** as
a per-message object with fixed section order; the **machine log** as a destination; one action
vocabulary across keys, gestures and palette; state as words plus colour; a token set authored as two
equal ramps rather than one ramp inverted. Web-only: the chip/text duality of the search field, the
three-pane layout, settings-as-route riding on browser history.

**What would have to be true.** That the score is good enough to fold on but not to reorder on, which
really means users do open folds, since a folded message is only recoverable if someone looks. It can
check itself locally without telemetry: the fold header states how many of last week's folded
messages were later opened.

**Server work.** The machine log needs a user-facing record of automatic acts with causes and undo.
An audit log table exists for system actions (purge, hard delete) and is not exposed; the change log
records object mutations without a reason. This is new server work and the direction must carry that.

### 3. Lenses over one list

*Mid tier, where it was called "The Ledger"; renamed here to avoid a collision with direction 4.*

**Thesis.** A single, ordered, auditable record of everything in every account. Priority, arrival and
category are not places you go; they are lenses applied to the same list, the way a ledger is re-sorted
by date, by amount or by account without ever leaving the ledger. Every row can justify itself, and
every judgment the system makes about a row is inspectable and correctable in place. One surface,
always the same shape, that never asks to be trusted blindly.

**Optimises for** a single stable mental model (one list, one row format, one place your hands know,
whichever lens is active) and trust through visible reasoning. **Gives up** the chance to tune any one
mode for its own job (a single row format serving arrival, priority and category equally is a
compromise for all three) and a calmer first impression: score, category confidence and rule matches
are visible by default, a deliberate bet against a quieter surface.

**First screen.** A dense one-line-per-thread list, unified across accounts, in arrival order by
default because that ordering needs no explanation. Rows: sender, subject, snippet, a small
account-colour mark, label chips, a compact indicator for score, category and lock state. Directly
above the list a lens strip (Priority, Arrival, Category, plus any pinned saved search) switched by
number key or click with no page transition. One keystroke away: switching lens, the reasoning behind
the selected row, compose, search, a command palette. Buried behind one small constant entry point:
account health, the rules editor, key management, export.

**The three views.** Orderings of one list. The priority lens groups the same rows into bands
(important, everything else, low) inside the list; the category lens groups by function, with
needs-a-category pinned at the top of that lens as a band with a live count whenever it is non-empty;
the arrival lens is the same rows unbanded. Opening the needs-a-category band drops into a focused
decision mode without leaving the list.

**Settings.** One small constant entry point opens a single searchable index of every settings object
(accounts, identities, signatures, rules, saved searches, labels, notification preferences, export)
rendered as rows in the same visual language as mail. Typing filters instantly; there is no tree to
learn. The same key that opened it closes it and returns to exactly where you were.

**Keyboard and phone.** A small consistent set of single-key actions: move selection, lens by number,
mnemonic letters for archive, snooze, mute, label, reply and the unsubscribe surface, a key for
reasoning, a key for the palette, Escape to back out one level. Multi-select and bulk action, including
select-all-matching-search, work identically whichever lens produced the list. Phone: the lens strip
becomes a swipe or a small fixed bottom control; rows compress to two lines; reasoning and palette
move into bottom sheets; the one or two most frequent actions become row swipes.

**The awkward things.** A withheld attachment is a labelled block inside the message body (verdict,
reason, type, size) with a single release action gated by a confirmation stating what release means
(this file, and every copy of it). Encrypted and undecryptable mail get a lock glyph in the row, and
the same reasoning mechanism answers "why can't I read this". Account state is a persistent band at
the top of the list whenever not nominal, because it must be unmissable from the surface people look
at daily. A collapsed conversation is one row that expands in place. A queue at four hundred switches
into a mode that clusters by sender or domain and asks one decision per cluster.

**Backlog.** Nudges: another band in the priority lens. Suggested rules: through the per-row reasoning
panel ("this looks like a pattern; make it a rule?"). Agent confirmations: their own lens,
structurally identical to needs-a-category and inheriting its clustering. Archive cleanup: the same
clustered mode pointed at an import. LLM summary: a dismissible line above the message, simply absent
where not installed; smart reply as chips near the reply control under the same rule. Calendar: an
inline accept/decline block in the same family as the withheld-attachment block. Shared mailbox:
another account-coloured lane in the already-unified list.

**Carry-forward.** Carried: one list with switchable lenses, the row vocabulary, the reasoning panel
as a universal explain-and-correct mechanism, settings as a searchable index. Web-only: the exact
chord set, the placement of the settings entry point, swipe-to-switch-lens.

**What would have to be true.** That a technical user genuinely prefers one consistent, always-legible
surface over several quieter, purpose-built ones; that a single mental model outweighs the cost of
showing scores, rule matches and category confidence everywhere. If visible machinery reads as noise
even to this audience, the honest fix is separate quieter destinations, the opposite of this
direction.

**Server work.** None.

**A variant the frontier rejected.** The cheapest tier's believed-in direction ("Explained Priority")
is this same single list **sorted by score**, with an explanation card one keystroke away and arrival
as an alternate view. Both frontier directions in this family explicitly refuse sort-by-score: score
order jitters as the model updates and hides what is new. The explanation card itself, however, is
something every tier converged on.

---

## Family II: a different primary object

Three directions refuse the message list as the resting state of the app.

### 4. Ledger, Not Inbox

*Frontier tier. The deliberately uncomfortable direction.*

**Thesis.** The client has no inbox. Mail does not wait in a list; the system files every message the
instant it arrives (priority bucket, category, labels from accepted rules) and writes one line to a
ledger for each decision, with its reason and a one-key reversal. The user's work is not triage but
audit: read the ledger, reverse what is wrong (which trains the model), and settle the items the
system could not decide, which gather in a pending section at the top. Reading a message is something
you do from an entry or a search, never the first thing you see. This inverts who is in charge, and
offers it to people who distrust machines acting on their mail on purpose: a visible, explained,
reversible action is a stronger form of accountability than a queue of permission requests, and the
machine already decides priority and category today in places nobody looks.

**Optimises for** time until nothing is pending; one grammar for every unusual capability this server
has (a scored explanation, an abstaining categoriser, proposed rules, held attachments, held sends,
undecryptable mail and account faults are all either decisions or pending items); a surface that
cannot be blank without saying so. **Gives up** the inbox as a reading place (a reverse-chronological
list exists only as a search one key away); **unread as a concept** (the ledger has pending and
not-pending; badges become pending counts; someone who uses unread as a to-do list must pin or snooze
instead); immediacy for everything-else mail (arrivals below the threshold fold into one line per day:
"41 filed today: 12 receipts, 20 notices, 9 lists"); and familiarity: a new user sees a log and a
queue, not mail.

**First screen.** One column at every size; a reading pane opens beside it on a wide screen. At the
top a status line that always shows its clock: "Last entry 40 s ago. 2 accounts syncing. 3 pending. 17
sends left today." If an account is paused or failing, the phrase becomes the fault in words and
cannot be dismissed; because the ledger always has a last entry and a clock, an empty ledger with a
fresh clock is truthfully quiet and one with a stale clock is visibly wrong. Then Pending, if anything
is: items each stating what it needs ("14 messages need a category"; "invoice.pdf from X held: could
not be scanned"; "Sending to Z in 24 s"; "First mail to your bank alias from a domain resembling one
you write to"), each opening into the smallest interaction that settles it. Then the Record, one line
per decision, newest first: time; what arrived (sender, subject, the alias it was sent to); what was
decided (a strong mark for important, the category word, labels applied by rules); why, as a clause
("rule: receipts from this domain"); the reverse key. The user's own actions are entries too ("you
re-filed X as receipt"; "you sent to Z as this alias, derived from the message you replied to").

**The three views.** None is the inbox. Arrival order is the ledger's order, but its unit is a
decision, not a message. Priority is the actor: it decides which entries interrupt and which fold into
the day line. Category is the filing destination named in every entry. Needs-a-category is the first
pending group, because it is precisely a decision the system declined to make. A correction is a new
entry, not a silent edit: "you moved X below; the model will learn from this". Three re-files of one
sender make the induced rule appear later as a pending proposal with its preview. The user watches the
model learn, entry by entry.

**Settings.** The status line is the door: the accounts phrase opens Accounts, the pending count opens
Pending, the sends phrase opens the outbound ledger. Everything else is one page, "how the system
decides", whose sections (Rules with fire counts, Categories, Priority shown as its effect on the last
seven days in counts, Notifications, Identities grouped active and dormant, Keys, Saved searches,
Export and deletion) each show their last effect, because it is the same data as the ledger. The
search field also finds settings and verbs.

**Keyboard and phone.** The ledger is a list; one reverse key applies the inverse decision and writes
the new entry; numbers re-file by category; a leader key jumps to a saved search or a pending kind.
Phone: Pending is the first screen and the Record the second; the phone exists to settle pending
items and read important entries; swipe reverses an entry or opens a pending item's settling action.

**The awkward things.** Withheld attachments are pending items by nature, with the same
explain-then-release interaction and the release written as an entry. Undecryptable mail is a pending
item ("could not decrypt: encrypted to a key you do not hold") with two settling actions. Account
state is a pending item that persists while the fault does, and a phrase in the status line.
Conversations are an entry per thread event ("3 new in thread X"), so the ledger collapses threads by
construction; mute writes an entry and the thread's future events fold into the day line. Four
hundred pending reads "400 need a category; 38 senders account for 310" before anything is opened,
offers clusters each settled with a category and optionally a rule with preview, and records the bulk
decision as one entry with a count, reversible as one.

**Backlog.** Nudges, agent confirmations and calendar invites are all pending items, identical in
shape to a send in its hold window. An LLM summary is a labelled clause in a thread entry. A shared
mailbox is another actor in the ledger; entries name who acted and where.

**Carry-forward.** Carried: the entry grammar (what, decided, why, reverse); the three entry kinds
(decision, pending, fault) and their tones; the rule that a correction is an event; pending-first on
the phone; the status line with its clock; the held, released and seal vocabularies. Web-only: the
merged search-and-command field's keystroke behaviour, the wide-screen reading pane, day and week
folds as scroll behaviour.

**What would have to be true.** That this user experiences a reversible, explained action as more
control than a permission request, and that reversals are rare. If more than a small share of entries
need reversing, the ledger becomes a log of regrets and the user will want the inbox back; if the
user reads the ledger and feels supervised by their own client, no amount of transparency saves it.

**Server work.** Two additions, which the source names and which change the milestone's premise: the
Record of the user's own actions must live on the server to be identical on every device, and the
category clause needs the server to name the rule that fired. Today the category verdict is stored
without the rule that produced it (ADR-022), so the clause degrades to "by rule". The release API,
which the source assumed was missing, exists.

**A variant from the cheapest tier.** "Routed Inbox" makes the same inversion (the inbox is only
what did not match) but puts the **rules editor** at the centre rather than a decision log:
categories and labels are destinations in a sidebar, rules show their match counts, and the
needs-a-category queue is the landing page. Its own kill condition: users must be willing to maintain
rules, or induced rules must capture most of the work up front, or a new import lands almost
everything in the queue and demoralises rather than clarifies.

### 5. Correspondents First

*Near-frontier tier. The deliberately uncomfortable direction.*

**Thesis.** There is no list of messages. The default surface is a directory of correspondents, one
row per standing relationship, usually identified by the alias it uses. A user of a catch-all domain may have manufactured a
private address per service, so the mailbox is already partitioned by hand; this direction admits
that rather than asking a classifier to rediscover a structure the user declared. Messages live inside
correspondents. Mail becomes a set of channels with states rather than a stream with an ordering, and
the two hardest questions, which address do I reply from and what kind of thing is this, are answered
by the container instead of by a model.

**Optimises for** relationship-level control (mute a service forever, not a thread), structurally
unambiguous reply-from, a training signal the user can see, and compression: thousands of messages
become a few hundred rows, most never needing to be opened. **Gives up**, severely: the day ("what
arrived this morning" becomes a secondary view you must ask for; for anyone whose work is triaging a
day, an amputation); mailing lists and strangers (a list with forty posters is one correspondent with
poor internal structure, and a stranger's first email makes a row with no history, so the moment you
have least context is the moment this shape helps least); cross-cutting recall ("the PDF from that
thing in March" becomes purely a search problem); and familiarity.

**First screen.** A directory. Full-width rows, one per correspondent: name; the alias it uses as the
row's second identity; a preview of the latest message; an unread count; its states (muted, snoozed
until, awaiting your reply, held file, unverified signature, unsubscribe available). Three bands,
always in this order and labelled with counts: **Needs you** (unanswered, nudged, held, flagged),
**Active** (anything unread), **Quiet** (everything else, collapsed and searchable). Inside a band the
order is the priority score, the only place the score acts, and it acts on relationships. Above
everything a persistent account strip: one row per account with colour, sync state and lag, never
collapsed and never empty, so an instance with a dead account looks like one. One keystroke away:
search across all messages (the escape hatch, which must be excellent); compose; the **arrival
overlay**, a dismissible plain chronological list; the correspondent's record. Opening a correspondent
gives its history newest-first, its identity (alias, signature, key status) and its controls: mute,
unsubscribe, always-allow images, set category, block.

**The three views.** All demoted beneath a fourth axis and each re-cut. Category becomes a property
of the correspondent, decided once and applied past and future, so thousands of per-message
classifications become a few hundred stable human decisions. Priority becomes intra-band order and an
anomaly flag: a correspondent rises when something unusual *for it* arrives (a bill from a service that
only ever sends receipts). The model's question changes from "is this important", which needs to know
the user's life, to "is this unlike this relationship's normal", which needs only history; the source
calls this a claim about the model, not a certainty, and the first thing to check. Arrival survives as
a one-key overlay, deliberately shallow. Needs-a-category becomes **unfiled correspondents**, a band
at the foot of the directory; each decision resolves every past and future message from that
correspondent at once, so abstention stops being a growing backlog and becomes a bounded onboarding
step.

**Settings.** Two tiers, and the split is the design. Anything configuring a *relationship* (identity,
signature, category, notification behaviour, image policy, key, unsubscribe) lives inside that
correspondent. Anything configuring the *instance* (accounts, sync scopes, keys, notifications,
export, hard delete) lives in one destination reached from the account strip. Rules and saved searches
cut across relationships, so they sit at instance level, though each rule also appears on every
correspondent it touches. The honest weakness: per-object settings are invisible when you are not
looking at the object, so the instance page carries a search indexing both tiers that deep-links into
a correspondent's panel.

**Keyboard and phone.** The directory is the navigable object; inside a correspondent the message
keys mirror the directory keys, one verb set at both levels; one key toggles the arrival overlay from
anywhere; search results group by correspondent with a flatten toggle; number keys jump to bands. On a
phone this direction is *better* than on desktop, which the source offers as an argument in its
favour: a directory of counted, thumb-sized rows is the shape phones are best at; a correspondent is a
full-screen conversation; back is the system gesture; the overlay is a pull-down.

**The awkward things.** Held files surface on the correspondent row as well as in the message, because
the relationship is the unit of trust; release is per file with the same plain sentence, and this
shape can offer what a message-centric client cannot: release future files from this correspondent
automatically, visible on the record, revocable, stated as the risk it is, and offered only after a
second or third manual release. Key status is a property of the relationship, shown once; because you
compose *inside* a correspondent, encryption state is known before you type. The account strip is the
home account state currently lacks, and its healthy state shows when it last synced so "fine" carries
evidence. A thread is a run of messages inside a correspondent; a mailing list is one correspondent
with per-thread sub-rows, the direction's weakest joint, admitted rather than styled over. Four
hundred unfiled messages arrive as perhaps forty unfiled correspondents: an afternoon rather than a
month, the strongest single argument for the direction.

**Backlog.** Nudges are already a row state ("awaiting your reply, six days"). Agent confirmations: a
system correspondent, since the agent is a counterparty that asks you for things. LLM summary: at the
top of a correspondent, summarising the relationship. Calendar: an action strip plus a row state.
Shared mailbox: a second row in the account strip; correspondents gain an acting-as identity. Bulk
unsubscribe and newsletter bundling are nearly free (select rows in Quiet). Split inbox is already
present: the bands are it. No natural home: server-side filter editing and the vacation responder,
which will sit in the instance page looking like guests.

**Carry-forward.** The real payload is the object model: correspondent as primary noun, alias as its
identity, message as a child; category and key status as relationship properties; the three-band
ordering; settings attached to the object they govern; states written on a row rather than encoded as
badges. A native phone app inherits this almost unchanged; a desktop client has to earn density back,
probably with a correspondent's messages permanently in a second pane. Web-only: the overlay's
transient behaviour and the two-pane rendering.

**What would have to be true.** That the mailbox is a set of durable relationships rather than a
stream of one-offs: most arriving mail from correspondents already in the directory, and the alias
mapping close to one service, one address. The source asked for this to be measured on the intended
user's mailbox before anything is built, and to kill the direction rather than tune it if the long
tail dominates.

**Server work.** Category as a property of a correspondent with inheritance to its messages does not
exist (categorisation is per message today); relationship-anomaly scoring is a change to the model,
not the client. Both break the milestone's premise and the direction says so.

### 6. Single File

*Mid tier. The deliberately uncomfortable direction.*

**Thesis.** A decision engine, not a mailbox browser. It hands the user exactly one message at a time,
in priority order, and will not reveal the next until the current one has been explicitly disposed
of. The inbox is not a place to be scanned; it is a queue to be emptied. Browsing and searching exist
as a separate mode entered on purpose, never as the app's resting state.

**Optimises for** decision quality and an unambiguous training signal: every advance is an explicit
act (archive, snooze, mute, label, delete, reply, or an honest skip), so nothing sits in limbo and the
model gets a correction on every message rather than only the ones the user happened to touch.
**Gives up** the single most common thing a mail client is used for by a person with thousands of
messages and dozens of labels: scanning forty subject lines at once to spot the one that matters. It
also gives up casual bulk speed; clearing two hundred newsletters in two seconds requires leaving the
queue for browse mode first.

**First screen.** The single highest-priority message, filling the screen, with a thin strip above
showing how many items remain and which ordering produced this one. One keystroke away: the disposal
actions, always the same letters; "why this is next"; a skip that demotes rather than dismisses, kept
visibly distinct from archive. Buried behind one deliberate mode switch: the browse list, search,
settings, account health, compose-from-scratch.

**The three views.** Orderings chosen once per session rather than switched moment to moment,
because changing the whole ordering under a user mid-decision is disorienting in a one-item flow.
Priority is the default queue; arrival and category are alternatives picked before starting, or
pinned as a preference. When the needs-a-category backlog is non-empty, opening the app offers a
choice of which queue to work before dropping into single-item mode.

**Settings.** A full mode of its own, entered and exited by one action, with the long tail found by
typing. The one rule it cannot break: leaving settings returns to the exact message you were
mid-decision on, never to the top of a queue.

**Keyboard and phone.** Queue mode has roughly eight keys and no chords, because deciding what to do
with the message on screen must never require hunting for a command; browse mode carries the full
vocabulary. On a phone this direction changes the least of any, because one message per screen is
already how a phone presents mail; swipes take the two or three most frequent actions.

**The awkward things.** A withheld attachment fills the panel as a blocked section and its release is
a queue decision like any other. Undecryptable mail suits this model: an unavoidable single-screen dead
end ("this cannot be read: no key held") rather than a glyph a distracted user scrolls past. Account
state is the one thing this model handles poorly on its own terms, since a queue can only show what
has arrived, forcing a concession: a status strip visible even inside queue mode, the one piece of
permanent chrome the premise has to accept. A conversation is one queue item with an internal
sub-scroll, so a forty-message thread cannot monopolise a session. Four hundred pending becomes its
own session type with the same clustered escape hatch any large backlog needs.

**Backlog.** Nudges resurface as queue items. Suggested rules are a distinct item type interleaved
with mail. Agent confirmations are the best fit anywhere in the backlog, each request being exactly the
yes/no decision the app exists to present. The archive-cleanup assistant sits badly here and the source
says so: it is an overview tool and this direction resists overview, so it lives entirely in browse
mode. An LLM summary is offered *before* the user commits to reading, a natural fit. A calendar invite
is an accept/decline decision. A shared mailbox has no obvious home in a strictly personal queue.

**Carry-forward.** The one-item explicit-disposal loop and its small fixed vocabulary are the idea
meant to travel, and arguably suit a phone and a focused desktop client better than a browser tab.
The status-strip concession is required chrome in every form factor. Web-only: very little; the browse
list is the part most likely to be rebuilt per platform.

**What would have to be true.** That users want to be made to decide rather than allowed to defer. If
this user's real behaviour is closer to "let me see forty subject lines and pick the interesting
one", which is plausible for someone who already organises with dozens of labels, this direction
solves a problem the user does not have, and the discomfort it produces is not the productive kind.

**Server work.** None.

---

## Family III: written from a reader's reaction

A reader of the six described what they want instead. It is not any one of them; it is a
hybrid of three ideas the exploration kept apart, plus one thing none of them centred. This is what
"not that, but the middle bit" looks like written down, and it is the direction #372 wireframes
first.

What the reader said, in substance: there must be an inbox. It should be prioritised, either as two
tabs (review first / less important) or as one list with the less important filtered into another
area. Categories should partition everything, so that creator updates sit in one area, newsletters
and marketing somewhere ignorable, and shopping and shipping notices in a dedicated area. The
client's job is to help decide what is actually important, to make the important mail easy to keep
track of, and to shuffle the rest away while keeping it reachable. Mail that still needs addressing
today stays read-but-unarchived and gets lost; an explicit flag would be better. Archive removes a
message from the inbox, which is the catch-all term for unactioned mail; archived mail still appears
under All mail, under its label, and in search.

### 7. Desk and Drawers

*Written by the orchestrating model from that reaction, not by an independent designer.*

**Thesis.** The inbox is a desk. Everything on it is unactioned; archive clears it to the cabinet,
where nothing is lost. The desk has three parts. An **in-tray** at the top holds what you have
explicitly marked as needing you. The **work surface** holds new mail the model rates as worth
reviewing first, sorted into **areas** by category. The **drawers** hold everything else, sorted into
the same areas but closed, one line per drawer with a count, opened on demand and emptied in one
motion. Categories partition everything; priority decides whether a message lands on the surface or
in a drawer; and a third state, set only by you, decides whether it is in the tray. Inbox zero means
the tray and the surface are empty. The drawers are allowed to be full.

**Optimises for** knowing at a glance what needs you, never losing something you meant to come back
to, and making the unimportant majority cheap to ignore and cheap to clear. **Gives up** the plain
chronological inbox (arrival order survives only inside an area, and as All mail), the idea that
unread means anything about what to do next (unread stays a visual cue; it is no longer the to-do
list), and a little of the model's ambition: the score decides surface or drawer, never the order
you read in.

**First screen.** Wide: a rail (Inbox, In-tray, All mail; then labels, saved searches; then accounts,
each with a state word), the desk, a reading pane. The desk header states its own condition:
"Inbox: 3 in tray, 6 to review, 37 in drawers. Synced 40 s ago." A paused or failing account
replaces that sentence in the warning tone. Then the **in-tray**: rows you flagged, each carrying
the reason you gave it (reply, waiting, do), plus returned snoozes and bounces. Then the **work
surface**: areas as labelled sections with counts (for example Personal,
Notices, Creators, Social, and Needs sorting for new senders the classifier declined to file), each
area's rows in arrival order with sender, subject, snippet, arrived-at alias in muted type, thread
count, time. Then the **line**: "6 to review, 37 in drawers", with a handle whose stricter and looser
steps preview their effect in counts before anything moves. Then the **drawers**: one row per area,
closed ("Promotions 24, Newsletters 8, Social 5"), each with an open control, an archive-all
control, and an optional self-emptying setting ("archives itself after 14 days"). The same structure
can be drawn as two tabs, Review first and Everything else, with the in-tray pinned above the tabs;
the wireframes show both renderings so a reader can pick.

**The three views.** Category is the partition and is a place, but a place inside the inbox rather
than a destination you leave the inbox for: an area is a section of the desk and a drawer beneath it.
Priority is the line between surface and drawers, corrected by moving a message across it. Arrival is
the order within an area, and the whole of All mail. The needs-a-category queue is an area on the
surface called Needs sorting; placing a message there is a per-sender decision that becomes a rule,
so the area empties as the rules accumulate rather than refilling forever.

**Areas.** The server's five tabs are the default areas. A label that is mapped to a tab can be
promoted to an area of its own, which is how creator updates and shopping notices, both in the
Updates tab today, become two areas: the labels already exist. Every message has exactly one area;
a message with no rule and no verdict is in Needs sorting until placed.

**The in-tray flag.** One key marks a message as needing you and asks, optionally, why: reply,
waiting on someone, do. The flag is yours alone; the model never sets it. Nudges, when they arrive,
land in the tray as returned rows. Clearing the flag or archiving removes the row from the tray. The
tray shows its count in the rail and in the header sentence, so it nags without being opened.

**Archive is done.** It removes the message from every part of the desk and touches nothing else:
the area, labels and thread are kept, and the message is in All mail, under its label, and in search.
All mail has the same area filters as the desk and a switch to show or hide archived mail.

**Settings.** One searchable page whose sections show state (the converged answer): Accounts with
sync state in words; Areas, listing each area, the labels and tabs that feed it, and how many
messages are in Needs sorting; Priority, with the threshold shown as its effect on the last seven
days; Rules with fire counts, including the self-emptying drawers; Identities grouped active and
dormant; Signatures and templates; Notifications; Keys; Saved searches; Export and deletion. Escape
returns to the desk where you were.

**Keyboard and phone.** The desk holds focus; single letters are verbs on the focused row (archive,
flag for the tray, snooze, mute, reply, label); numbers are areas and, on the focused row, re-file
it; a chord moves a row across the line; a leader key jumps between tray, surface, drawers, All mail;
slash searches; question mark shows the map. Phone: a segmented control across the top (Tray 3,
Review 6, Drawers 37) replaces the three regions; areas become collapsible sections; swipes are
archive and flag; the drawers open one at a time.

**The awkward things.** Held attachments, encrypted and undecryptable mail, account state,
conversations and the four-hundred queue take the converged answers: hollow tiles with the reason in
words and a release that states its scope; seal state in words in the reading header; the header
sentence, rail word and empty state for account faults; a row is a thread; Needs sorting at four
hundred opens with a cluster report and per-cluster placement with rule previews.

**Backlog.** Nudges land in the tray. Agent confirmations land in the tray, since they are things
that need you. An LLM summary is a folded line above an opened thread, absent when not installed. A
calendar invite is a card in the reading pane and a tray row until answered. A shared mailbox is
another account colour and, if wanted, a second desk. Bulk unsubscribe is an action on a drawer.
Split inbox is the surface and the drawers.

**Carry-forward.** The three regions and their names; areas as the one categorisation vocabulary;
the flag with its three reasons; archive as done; the header sentence; the converged vocabularies for
held, sealed and returned. Web-only: the three-pane layout, the tabs-versus-list rendering choice,
the leader-key grammar.

**What would have to be true.** Three things, in order of risk. First, that the user uses the
flag: if the existing flag, pin and snooze go unused, the tray must be cheaper than leaving a message
unread (one key, a visible count, a reason that is optional) or it becomes another unused tool.
Second, that the line produces a surface small enough to clear daily: that depends on the
threshold and on daily volume, which is for each mailbox to measure. Third, that areas cover the mail:
whatever share of the inbox has no mapped label starts in Needs sorting.

**Server work.** The structure needs none: areas are labels and tabs, the line is the existing
threshold, archive and All mail are existing folder semantics, and the flag can ride the existing
flagged state with a label or keyword for its reason. Two small things to check before wireframes
become plans: a per-user "which labels are areas" setting, which is the same small addition #375
already anticipated; and whether the rules engine can express "older than N days" for self-emptying
drawers, or whether that is a job.

**Settled in pass-1 wireframes (#372, three rounds).** Two tabs (Review first,
Everything else) over one desk. The in-tray is a bounded strip above the tabs: one line closed
(count, oldest wait, how many over a week), at most five rows open, the full tray as its own view
grouped by wait; one flag, no reasons. Areas as listed, the five tabs as defaults, a mapped label
promotable to an area. Drawers **automatically archive** after a per-area period, default 30 days
for new areas, printed on the drawer; automatic archiving never deletes and never marks read.
Several accounts under one login, unified by default, with a switcher folded to one line that
scopes the whole desk; the app opens to the scope last used on the device. Settings: twelve rows,
each stating its state first, one setting in one place, scope (login, account, area, device) printed
on the row, per-account and per-area settings inside the account or area, one "more" per section.
The reader found it much closer to what they had in mind, with nothing in the wrong place.

**What it asks of the server, checked against the code.** Three things, filed as server issues in
the milestone because the milestone's premise (no new server capability) does not survive this
design: per-login desk preferences (which labels are areas, their order, each drawer's period and
the default, the line's threshold); a drawer auto-archive job, because rules evaluate a message
when it arrives and have no notion of age, so "older than 30 days" is a scheduled job in the shape
of snooze wake, and it must archive and do nothing else; and a timestamp on the flag, because the
tray shows how long an item has waited and the flag today is a boolean. Everything else exists:
per-account and per-tab query filters, thread collapse, attachment release, snooze wake markers,
sender warnings, seal state, account health.

**Settled in pass-2 mockups (#372, six rounds).** The structure dressed once, in both
themes side by side. A cool paper-grey ground with the list as a lighter sheet on it, three tone
steps and hairlines for all layering, no transparency; one verdigris accent spent on the line, the
focused row and the active tab; two account colours and otherwise no colour on rows; four semantic
tones (warning, held, sealed, returned) that always travel with a word; unread as weight, not colour;
IBM Plex Sans for the interface, IBM Plex Mono for counts and keys, Source Serif 4 for the message.
Decided along the way: actions are buttons with nothing appearing on hover and no keys printed on
them (the Keys button is the one exception); the reading pane resizes and a focused-reading option
takes the main area; the "why here" explanation retires after two reads of a sender into a sender
popover; a conversation folds earlier messages to one line each; key-changed and invalid-signature
warnings sit in the seal line's slot; areas are labels, placing applies the area's label, and `in:`
is a search facet; Appearance syncs per login within a client family; compose states From as a
sentence and the send line says what will happen, with no send-quota language; the Outbox exists in
the rail only while something is queued, Sent is permanent, Drafts appears while one exists; a held
attachment opens to an explanation rather than to itself; undo is a toast for every desk action;
offline and stale are two different sentences in the header; day one turns a folder-sorted mailbox
into a desk with a 30-day newly-arrived window, folders feeding the desk, older mail labelled and
archived without a server move, and rules induced from folders; archive is a server-side move; the
in-tray rides the IMAP flagged state; existing category verdicts are materialised as classifier-
applied labels, not mirrored to IMAP by default; screen-reader completeness is proven in CI.

**Versions.** The rebuild became four milestones: v1.65.0 desk foundations (server
objects, routing shell, token inventory), v1.66.0 the desk, v1.67.0 settings, compose, outbox and
viewer, v1.68.0 day one, accessibility and React 19. PGP encrypt/decrypt moved from v1.66.0 to
v1.69.0 as a consequence.

**Where it sits in the matrix.** The score partitions (surface versus drawers) with arrival order
inside, as in Above the Line. Category is a place inside the inbox, closer to the cheap tier's
"destinations" than to any frontier direction, but a place you never leave the inbox to reach.
Needs-a-category is an area on the surface. Account state is the header sentence and the rail. Unread
survives as a cue; the to-do state is the flag. A correction is crossing the line. You read in the
pane. Split inbox is the surface and the drawers.

---

## Where they converge

Where the cheapest and the most capable tier agree without seeing each other, the agreement is
probably the baseline any direction inherits.

1. **Settings is one searchable page that shows state**, never a menu plus modal sheets. All six.
   Two of them want the search to index *values* (an alias finds its identity, a sender finds its
   rule), because the long tail is remembered by content, not by label.
2. **Account state must intrude on the primary surface**, beside a timestamp: a header sentence, a
   band, a strip, a pending item. Five of six; the one that kept it inside an Accounts page was the
   cheapest tier. Every list needs a clock and a sync sentence, because an empty list without one is
   indistinguishable from a broken one.
3. **Explain-and-correct is one mechanism**, reachable from wherever the machine's judgment is
   visible: score and why, category and why, rule that matched, verdict. All six.
4. **Needs-a-category is visible where you already look**, never a place to remember, and at four
   hundred it clusters (by sender, domain, alias, near-miss rules) with per-cluster decisions and
   previews. All six.
5. **The row is a thread**, and a one-message thread is indistinguishable from a message. Every
   direction that addressed it.
6. **Withheld attachments** are "held" plus the reason in words; release explains its scope (every
   copy of the same content); flagged asks twice; the language never accuses, because the common case
   is a password-protected PDF and calling that dangerous teaches people to click through. Held,
   withheld and undecryptable are one interaction problem: content exists, you cannot have it yet,
   here is the door and what opening it costs.
7. **Reply-from is a stated fact with an override, never a picker**, and the alias a message arrived
   at is shown on the row as data, because for this user it is a signal about the message.
8. **The score is never shown as a number**; correction is a crossing between buckets, and the
   threshold is a separate control whose effect is previewed in counts before anything moves.
9. **Colour is never the only carrier.** Colour is spent on accounts and labels; category is a word;
   warning, held, sealed and returned are semantic tones defined equally in both themes; no
   transparency.
10. **One vocabulary, several doors.** Whatever a desktop key does, a phone gesture and a palette
    entry do under the same name, or the native apps ship three dialects of one client.
11. **Every empty state carries a timestamp** and says why it is empty. "Nothing since 09:14, last
    synced two minutes ago" is a claim that can be wrong, which is what makes it useful.

## Where they disagree

These are the decisions, independent of style. A direction is a bundle of answers to them; the
reader may prefer a different bundle.

| Decision | Above the Line | Intact Timeline | Lenses | Ledger, Not Inbox | Correspondents First | Single File |
|---|---|---|---|---|---|---|
| What the score does to the list | partitions it (above / below / low); arrival order inside | only folds low mail at its own timestamp; never reorders | one of three switchable lenses (bands) | decides what interrupts and what folds into a day line | orders relationships within a band; flags anomalies | orders the queue |
| What category is | a word on the row and a filter | a fold label and a search facet | a lens | the filing destination named in each entry | a property of the correspondent, decided once | a session ordering |
| Where needs-a-category lives | one row under the line | its own fold with its own voice | a band pinned atop the category lens | the first pending group | the unfiled-correspondents band | a queue offered at session start |
| Where account state lives | header sentence, rail dot, empty state, Accounts | rail dot plus word, persistent line above the inbox, machine log | a band at the top | status line plus a persistent pending item | the account strip, never collapsed | a status strip, the one concession |
| Does unread survive | yes | yes | yes | **no**: pending / not pending | yes (per correspondent) | no: disposed / not yet |
| What a correction is | crossing the line; "moved by you" in the why strip | a change recorded in the machine log | in-place edit via the reasoning panel | a visible new entry | re-filing a relationship | an explicit disposal |
| Where you read | list plus pane | list plus pane plus record margin | list plus pane | from an entry or a search | inside a correspondent | one message, full screen |
| Server work required | none | a user-facing machine log | none | the user's own actions as a server record; the rule that fired | per-correspondent category; relationship-anomaly scoring | none |
| Phone | the list alone, line and header unchanged | four bottom destinations, record as a sheet | lens control at the bottom | pending first, record second | the directory: better than desktop | the queue: native to a phone |
| Split inbox | the line is the split; no more regions | deliberately no home | pinned saved searches | day line versus interrupts | the bands are it | not applicable |

Two further disagreements cut across the table. **Visible machinery or earned on request:** Lenses
and Ledger show score, category confidence and rule matches by default as a matter of trust; Above the
Line and Intact Timeline show position and a mark, with the reasons one key away; a calmer direction
than any of these is possible and nobody wrote it. **Per message or per relationship:** only
Correspondents First moves classification to the relationship, and the source calls that the
highest-leverage decision in the brief because it alone decides whether four hundred unfiled items are
a chore or an afternoon.

## Corrections to the sources

The directions were written without access to the code, on purpose. Four claims about the server
needed checking.

- **Attachment release is already client-callable.** Two directions assumed the server exposed no
  release; it does, as `Email/releaseAttachment` beside `Email.attachmentScan`. What is missing is only
  the client interaction. No direction needs server work for release.
- **The category verdict does not record the rule that produced it.** ADR-022 stores the classifier's
  verdict only, so "why this category" is answerable today only as "by rule", "by classifier" or "by
  you". The Ledger direction's server ask stands; any direction promising "category and why" inherits
  the same limit.
- **A user-facing machine log or record is new server work.** An `audit_log` table exists and records
  system actions (purge, hard delete); it is not exposed to clients. The change log records every
  mutation but carries no reason. Intact Timeline and Ledger both depend on something the server does
  not yet produce, and both say so.
- **The alias count can be larger than the brief says.** The brief gives the declared identities;
  on a catch-all domain the mailbox shows more distinct arrived-at addresses, because the catch-all
  derives addresses that were never declared. Directions that lean on the alias as structure lean on
  the larger number.

## Measurements

Two directions named measurements that would decide their fate. They are listed here as questions
to answer read-only on the intended user's own mailbox, each with its denominator; no figures from
any particular mailbox are recorded.

**Correspondents and aliases** (decides whether Correspondents First survives):

- distinct sending addresses, and how many carry one, two, three or more, and ten or more messages;
- the share of mail from correspondents with three or more messages, and from single-message ones
  (the size of the junk-drawer band);
- how many correspondents and how many aliases carry 80% of mail, and the share from the top twenty;
- the share of messages carrying a mailing-list header;
- the share with a recorded arrived-at alias, and the distinct alias count against the declared
  identity count;
- aliases by number of sending domains, and the share of mail on aliases shared by four or more;
- messages per day, and per day with score at or above 0.5, 0.7 and 0.8; the score distribution;
- threads with more than one message.

**The inbox today** (decides whether Desk and Drawers is sized right):

- messages in the inbox against all live messages, and how many are archived;
- unread against read-but-unarchived, and how many of the latter are older than 30 days;
- the inbox by age bucket (7, 30, 90 days);
- flagged, pinned, snoozed and answered counts;
- the share at or above 0.5 and the arrivals per day;
- the inbox by label, labels not mapped to any tab, and messages with no label at all;
- the split by tab from the label mapping alone.

**What these would and would not say.** They would say whether the mailbox is mostly durable
relationships, whether the alias structure is real, and how many messages a threshold surfaces a
day. They would say nothing about whether those messages are the right ones: the classification
epic (#220) found that respectable ranking coexisted with poor precision at the threshold that
decides what a user sees, so any direction that puts the score in charge is betting on a model
whose precision at the operating point has to be re-measured, not assumed.

## What this does to the provisional issues

The milestone's other issues were drafted before any of this existed. Their fate is decided by the
direction chosen, so none is rewritten here, but the effect of each direction is
already clear enough to record.

- **#375, let the user choose what "Inbox" means.** Invalidated in its current framing by all six:
  no direction makes "which view is the inbox" a stored preference. The question it was asking
  survives as the first row of the table above (what the score does to the list) and the second (what
  category is). Single File comes closest to the original, with a per-session choice of queue. Its
  proposal to pull "split inbox" out of #68 is answered differently by each direction (see the last
  table row).
- **#373, routing and a settings shell.** Survives under every direction: all six make settings a
  destination you enter and leave, and two make it a route that the browser's back button understands.
- **#374, settings pages for accounts, identities and signatures.** Reshaped under every direction:
  five make settings one searchable page whose sections show state rather than a set of pages;
  Correspondents First attaches relationship settings to the correspondent and keeps only instance
  settings on a page.
- **#376, a design system worth the name.** Survives and is sharpened: every direction wants two equal
  token ramps rather than one inverted, semantic tones (warning, held, sealed, returned) that hold in
  both themes, colour reserved for accounts and labels, and no transparency.
- **The quarantine-release interaction (ADR-027 section 8).** Every direction gives it a home and
  the same wording rules; the server half already exists.
- **The collapseThreads client half (#210).** Every direction makes the row a thread. Conversations may be rare on a given mailbox, which is worth checking.
- **#380, React 19 and TypeScript 7.** Unaffected by the choice; still held for after the components
  settle.

## What happened next

The seventh direction was the result. #372 turned it into structural wireframes (three rounds) and
then dressed mockups (six rounds) until the structure stopped moving. The provisional issues were
rewritten against the result, the surfaces the mockups added were filed as their own issues, the
server work the design needs was filed rather than assumed, and the rebuild was carved into
versions. The exploration is closed; the build starts with the server foundations.
