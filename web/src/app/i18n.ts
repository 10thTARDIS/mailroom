// SPDX-License-Identifier: AGPL-3.0-only

// i18n scaffolding (spec §12: "i18n-ready"). No user-facing string is hardcoded
// in a component: every one goes through t(), so adding a locale is adding a
// catalog, not auditing the JSX. Deliberately dependency-free — a full ICU
// library is a v1 decision, and the shape below (keys + {named} params) is what
// every such library consumes.

const en = {
  "app.name": "Mailroom",
  "app.loading": "Loading your mail…",
  "app.signedInAs": "Signed in as {email}",
  "app.skipToContent": "Skip to message list",

  "conn.offline": "Offline — changes are queued and will be sent when the server is reachable.",
  "conn.error": "Cannot reach the server: {error}",
  "conn.pending": "{count} pending",

  "nav.inbox": "Inbox",
  "nav.priority": "Priority",
  "nav.needsCategory": "Needs a category",
  // The category-tab strip (#228). "Primary" is the inbox itself, so it is the
  // strip label rather than a separate destination.
  "tabs.strip": "Categories",
  "tabs.primary": "Primary",
  "tabs.social": "Social",
  "tabs.promotions": "Promotions",
  "tabs.updates": "Updates",
  "tabs.forums": "Forums",
  // Arranging the strip and the mapping (#228). The wording carries two facts a
  // user cannot see for themselves: that a label BEATS the automatic sorting,
  // and that switching a category off does not hide its mail — it moves it.
  "tabs.settings": "Categories",
  "tabs.explain":
    "Categories sort your inbox by what mail DOES — an offer, a receipt, a notification — separately from the labels that say what it is about.",
  "tabs.notLoaded": "Your categories have not loaded yet.",
  "tabs.strip.heading": "The strip above your inbox",
  "tabs.primaryAlwaysOn": "Always on — mail from a category you switch off appears here.",
  "tabs.feed.none": "No labels point here; filled by sorting alone.",
  "tabs.feed.one": "1 label points here.",
  "tabs.feed.many": "{count} labels point here.",
  "tabs.feed.whileOff": "While it is off, that mail appears in Primary.",
  "tabs.stripHidden": "Only Primary is switched on, so no strip is shown above your inbox.",
  "tabs.moveUp": "Move {tab} earlier",
  "tabs.moveDown": "Move {tab} later",
  "tabs.mapping.heading": "What each label feeds",
  "tabs.mapping.explain":
    "A label decides its mail's category outright. Anything with no label pointing anywhere is sorted from what its headers say it is, which is a guess — a good one for list mail and newsletters, less so between offers and notifications.",
  "tabs.mapping.auto": "Sort automatically",
  // Short on purpose: this sits inside a dropdown that shares its row with the
  // label name, and every character widens the control's minimum size.
  "tabs.mapping.offOption": "{tab} (off)",
  "tabs.mapping.forLabel": "Category for the label {label}",
  "tabs.mapping.noLabels": "You have no labels yet, so there is nothing to point at a category.",
  "nav.starred": "Starred",
  "nav.snoozed": "Snoozed",
  "nav.unread": "Unread",
  "nav.sent": "Sent",
  "nav.drafts": "Drafts",
  "nav.archive": "Archive",
  "nav.trash": "Trash",
  "nav.junk": "Junk",
  "nav.all": "All mail",
  "nav.flagged": "Flagged",
  "nav.folders": "Folders",
  "nav.labels": "Labels",
  "nav.accounts": "Accounts",
  "nav.newLabel": "New label…",
  "nav.mailboxes": "Mailboxes",

  "search.placeholder": "Search mail — try: from:alice invoice is:unread",
  "search.label": "Search mail",
  "search.clear": "Clear search",
  "search.unavailable":
    "This view needs the search index, and this server has none (or it is disabled).",
  "search.help":
    "Operators: from: to: cc: alias: subject: body: label: area: list: is: has: in: filename: larger: smaller: account: before: after: on: date: newer_than: older_than: — prefix any with “-” to exclude; combine with OR and (brackets).",
  // #57. The suggestion list is now the real documentation for the operators; the
  // tooltip above stays for anyone who never opens it.
  "search.suggestions": "Search suggestions",
  "search.recent": "recent",
  "search.clearHistory": "Forget recent searches",
  // Saved searches / smart folders (#57). "Smart folder" is what a saved search
  // LOOKS like once it is in the rail, so the sidebar heading uses that word and the
  // search box talks about saving x2dx2d the same object, named for what you are doing.
  "search.save": "Save this search",
  "search.saveAs": "Save as",
  "search.pinToSidebar": "Show in the sidebar",
  "search.saved": "Saved {name}.",
  // The search box, #724: the options form, the sort toggle, and the bound.
  "search.options": "Search options",
  "search.options.title": "Search options",
  "search.options.from": "From",
  "search.options.to": "To",
  "search.options.subject": "Subject",
  "search.options.hasWords": "Has the words",
  "search.options.doesntHave": "Doesn't have",
  "search.options.within": "Date within",
  "search.options.withinAny": "Any time",
  "search.options.within.1d": "1 day",
  "search.options.within.3d": "3 days",
  "search.options.within.1w": "1 week",
  "search.options.within.2w": "2 weeks",
  "search.options.within.1m": "1 month",
  "search.options.within.2m": "2 months",
  "search.options.within.6m": "6 months",
  "search.options.within.1y": "1 year",
  "search.options.larger": "Larger than",
  "search.options.smaller": "Smaller than",
  "search.options.sizeHint": "e.g. 5M or 200K",
  "search.options.area": "Area",
  "search.options.areaAny": "Any area",
  "search.options.attachment": "Has attachment",
  "search.options.attachment.none": "Doesn't matter",
  "search.options.attachment.any": "Any attachment",
  "search.options.attachment.pdf": "PDF",
  "search.options.attachment.image": "Image",
  "search.options.attachment.document": "Document",
  "search.options.attachment.spreadsheet": "Spreadsheet",
  "search.options.attachment.presentation": "Presentation",
  "search.options.anywhere": "Include Trash and Junk",
  "search.options.submit": "Search",
  "search.sort.label": "Sort search results",
  "search.sort.newest": "Newest first",
  "search.sort.relevance": "Best match",
  "nav.smartFolders": "Smart folders",
  "savedSearch.manage": "Saved searches",
  "savedSearch.none": "You have not saved a search yet.",
  "savedSearch.explain":
    "A saved search runs the same query you typed. Show one in the sidebar and it behaves like a folder that fills itself.",
  "savedSearch.query": "Query",
  "savedSearch.deleteConfirm": "Delete the saved search {name}?",
  "savedSearch.notPinned": "not in the sidebar",
  "savedSearch.nameRequired": "Give it a name so you can find it in the sidebar.",
  "savedSearch.queryRequired": "It needs a query to run.",

  "list.empty": "Nothing here.",
  "list.loading": "Loading…",
  "list.loadMore": "Load more",
  "list.countOf": "{shown} of {total}",
  // #722/#724: the search index stopped at its hit limit, so the total is a floor.
  "list.countOfAtLeast": "{shown} of at least {total}",
  "list.countBoundExplain":
    "The search stopped after {total} matches. Narrow it to see the rest.",
  "list.selectAll": "Select all on this page",
  "list.selected": "{count} selected",
  "list.selectAllMatching": "Select all {total} conversations matching this view",
  "list.selectAllMatchingBound": "Select the first {total} conversations this search found",
  // #769: a bulk action over a search that matched more than one batch carries on.
  "list.nextBatch": "Done {done} of {total} — select the next {next}",
  "list.nextBatchNone": "That was all of them.",
  "list.selectionCleared": "Selection cleared",
  "list.noSubject": "(no subject)",
  "list.noSender": "(unknown sender)",
  // Why a row is showing an address it usually would not (#307). It appears on
  // the handful of rows whose display NAME is itself address-shaped, or was built
  // with a direction-override character — the two cases where showing the name
  // alone would state an identity the message does not have. Phrased as a fact
  // about this message rather than an accusation: plenty of legitimate senders
  // put an address in their display name.
  "list.senderAddressShown":
    "This sender's display name looks like an address. The address after it is the one the message was actually sent from.",
  "list.draft": "Draft",
  // #446. On an All-mail row: day-one setup took this message off the desk and
  // NOTHING MOVED — the bytes are exactly where the provider has them. Deliberately
  // not the word "archived" alone, which every other client would read as the
  // Archive FOLDER.
  "list.deskArchived": "archived in Mailroom",
  "list.attachment": "Has an attachment",
  "list.pending": "Change not yet confirmed by the server",

  "action.archive": "Archive",
  "action.trash": "Delete",
  "action.untrash": "Restore",
  // #528, spec §6: rescue a false positive from Junk. The provider's own word for
  // the gesture, not "Move to Inbox" — it is a verdict, and the move is how it is
  // delivered.
  "action.notJunk": "Not junk",
  "action.markRead": "Mark as read",
  "action.markUnread": "Mark as unread",
  "action.star": "Star",
  "action.unstar": "Remove star",
  "action.label": "Label",
  "action.reply": "Reply",
  // #646. The decision of who this goes to (Reply-To over From, plus the
  // parent's To/Cc minus the user's own addresses) lives in
  // ui/compose/recipients.ts.
  "action.replyAll": "Reply all",
  // #582. The composer for this shipped in #448; nothing outside the attachment
  // viewer opened it, so a message with no attachment could not be forwarded.
  "action.forward": "Forward",
  "action.compose": "Compose",
  "action.refresh": "Refresh",
  "action.back": "Back to the list",
  "action.undo": "Undo",
  "action.close": "Close",
  "action.cancel": "Cancel",
  "action.save": "Save",
  "action.edit": "Edit",
  "action.delete": "Delete",
  "action.send": "Send",
  // Send & archive (#648): beside Send, only for a reply/reply-all/forward —
  // ./ui/compose/sendArchive.ts offersSendAndArchive is the one place that
  // decision lives.
  "action.sendAndArchive": "Send & archive",
  "action.saveDraft": "Save draft",
  "action.print": "Print",
  "action.pin": "Pin",
  "action.unpin": "Unpin",
  "action.mute": "Mute conversation",
  "action.unmute": "Unmute conversation",
  "list.pinned": "Pinned",
  // What mute DOES (store.DivertMutedArrival), said plainly, including where it
  // stops: a reply that lands in the inbox is archived in Mailroom on arrival,
  // left unread, and sends no notification — and the server is not touched, so
  // any other mail app still shows it in the inbox. The old line ("won't be
  // surfaced") promised this while mute changed nothing but the banner itself.
  "thread.muted":
    "Muted — new replies go straight to Archive, unread, and won't notify you. That happens in Mailroom only: nothing moves on the server, so other mail apps still show them in the inbox.",
  "action.applyLabel": "Apply label",
  "action.snooze": "Snooze",

  // Priority inbox (#154). The engine is a per-user model, so the language is
  // about what IT thinks — never "this is important", which would claim a
  // certainty the score does not have.
  "priority.important": "Important",
  "priority.bucketImportant": "Important",
  "priority.bucketRest": "Everything else",
  "priority.markImportant": "Mark as important",
  "priority.markNotImportant": "Not important",
  "priority.why": "Why is this ranked here?",
  "priority.whyTitle": "Ranked {score} of 1.00",
  "priority.reasonUp": "raises",
  "priority.reasonDown": "lowers",
  "priority.unscored": "Not scored yet — the model learns from what you do.",
  "priority.learning":
    "Priority is still learning from your mail. Correcting it teaches it.",

  // The needs-a-category queue (#176). The register is a QUESTION throughout —
  // never "we think this is Promotions", which would be the system announcing a
  // decision it has not made and is not entitled to make. It is asking, and the
  // wording has to be recognisably an ask or people stop reading it as one.
  "queue.confidence": "{percent}%",
  // The fallback for a rule-sourced suggestion whose evidence phrase is
  // missing (#242) — reasons null or empty, which the server's own contract
  // for label_suggestions.reasons already concedes can happen (a row written
  // before migration 00019, or nothing recorded). NOT the normal path: a
  // rule-sourced suggestion always carries its evidence as the last entry of
  // `reasons` (suggestionConfidenceDisplay, NeedsCategory.tsx), and this string
  // exists only so that gap renders as an honest "this is unbacked", never a
  // blank confidence slot and never — under any circumstance — a percentage.
  "queue.confidence.ruleFallback": "matches a filing rule you have",
  "queue.unknownLabel": "a label you deleted",
  // The divider under the batches. Says what these rows ARE — mail that arrived
  // on its own — rather than "ungrouped", which describes the software's
  // difficulty rather than the user's mail.
  "queue.rest": "One at a time",
  "queue.dismissOne": "Does not need a category",
  "queue.dismissed": "{count} marked as not needing a category.",
  "queue.reasonFor": "suggests it",
  "queue.reasonAgainst": "argues against",
  // "No record of why" and "looked, found nothing" are DIFFERENT statements and
  // the server distinguishes them. Collapsing them here would put a claim in its
  // mouth that it never made.
  "queue.noReasonRecorded": "No record of why — this was suggested before reasons were kept.",
  "queue.noReasons": "The model could not say what led it here.",
  "queue.thread.question": "We are not sure what this is.",

  // Batch triage: one question, N answers. The alias comes first in the sentence
  // because it is the part the user recognises — "bank@" tells them what to
  // do; "24 messages" only tells them how much of it.
  "queue.group.askAlias": "{count} messages arrived at {alias} — where do these go?",
  "queue.group.askDomain": "{count} messages from {domain} — where do these go?",
  // When the list does not hold the whole queue, the count is a floor: the group
  // may continue past the window, and "Label all {count}" labels what is loaded.
  // Saying "at least" is the difference between a bounded number and a wrong one.
  "queue.group.askAliasPartial":
    "At least {count} messages arrived at {alias} — where do these go?",
  "queue.group.askDomainPartial": "At least {count} messages from {domain} — where do these go?",
  // Homogeneous or a crowd. Stated plainly rather than as a warning: a mixed
  // group is not an error, it is a fact the person needs before deciding whether
  // one label fits all of it.
  "queue.group.senders": "from {senders} different senders",
  "queue.group.label": "Label all {count}",
  "queue.group.dismiss": "None of these",
  "queue.group.labelled": "Filed {count} messages under {label}.",
  // Past tense and unambiguous: the card stays after an undo, so it has to stop
  // claiming the filing happened without leaving the user guessing whether the
  // undo worked.
  "queue.group.undone": "Took back {count} messages from {label}. They are back in the list.",
  "queue.group.undoFailed": "Could not undo that — the label is still applied.",
  "queue.group.checkingRule": "Checking whether this implies a rule…",
  // COLLATERAL IS A BLAST RADIUS, NOT AN ERROR COUNT. Unlabelled mail is not a
  // negative example — the user labelled one page to get started — so this says
  // what the rule would TOUCH and lets them look, and never scores it as wrong.
  "queue.group.ruleEffect":
    "A rule for these would keep filing them, and would also label {collateral} other messages you already have.",
  "queue.group.ruleEffectNone":
    "A rule for these would keep filing them, and matches nothing else you have.",
  // The same two, measured over the mail the server actually scanned rather than
  // the whole mailbox. A bounded count stated as a total is a claim the server
  // deliberately declined to make (it reports `complete` precisely so a client
  // does not make it).
  "queue.group.ruleEffectPartial":
    "A rule for these would keep filing them, and would label at least {collateral} other messages — counted over your recent mail.",
  "queue.group.ruleEffectNonePartial":
    "A rule for these would keep filing them, and matched nothing else in your recent mail.",
  "queue.group.noRule": "No rule fits these yet — they have too little in common.",
  // Distinct from noRule, and the distinction matters: the label has not been
  // used enough yet, which is not a statement about these messages at all.
  "queue.group.needMore":
    "Label {needed} more like these and a rule can be suggested for them.",
  "queue.group.notSaved":
    "Filed here, but not saved to the server yet — the rule check will run once it is.",
  "queue.group.ruleUnavailable": "Could not check whether this implies a rule.",
  "queue.group.reviewRule": "Review rule",

  // The empty state is a SUCCESS. "Nothing here" is what an empty Trash says,
  // and it is the wrong sentence for a queue that is empty because the system
  // is not stuck on anything.
  "queue.empty.headline": "Everything has a category.",
  "queue.empty.body":
    "Nothing is waiting on you. New mail that nothing has categorised will appear here.",

  // Snooze (#51). Every choice is shown with the instant it resolves to, so the
  // catalog carries the labels and the UI supplies the resolved time.
  "snooze.title": "Come back to this…",
  "snooze.laterToday": "Later today",
  "snooze.thisEvening": "This evening",
  "snooze.tomorrow": "Tomorrow",
  "snooze.thisWeekend": "This weekend",
  "snooze.nextWeek": "Next week",
  "snooze.pickTime": "Pick a date and time",
  "snooze.set": "Snooze",
  "snooze.unsnooze": "Bring it back now",
  "snooze.currently": "Currently set to return {when}",
  "snooze.mustBeFuture": "Choose a time in the future.",
  "snooze.noSelection": "Select a message first.",
  "snooze.returns": "Returns {when}",
  "snooze.returned": "Came back {when}",
  // ⚠ WAKING MOVES NOTHING (store.WakeSnoozed only clears snoozed_until). A
  // snoozed message reappears wherever it already lives — the top of your Inbox
  // and the desk, ordinarily, but if you filed it somewhere else while it was
  // snoozed, it stays there. "Returns to the Inbox" would be a promise the wake
  // itself does not keep.
  "snooze.until": "Snoozed until {when} — it reappears then. If you moved it, it stays where you put it.",

  "thread.messages": "{count} messages in this conversation",
  "thread.to": "To",
  "thread.deliveredTo": "Delivered to",
  "thread.preview": "Preview",
  "thread.attachments": "Attachments",
  // Remote content is blocked by default (spec §11). The affordance says what it
  // actually does. The SSRF-guarded image proxy (ADR-026) HAS SHIPPED and is on
  // by default — this string is what ThreadPane.tsx shows only when it is OFF
  // (`client.imageProxyEnabled()` picks between this and remoteBlockedProxied
  // below), in which case loading remote images still fetches them straight from
  // the sender's server and tells them you read this.
  "thread.remoteBlocked":
    "Remote images in this message were blocked. Loading them tells the sender you opened it.",
  // With the image proxy on (ADR-026), the same click means something different:
  // the SERVER fetches the images, so the sender sees the server's address and
  // learns nothing about the reader's browser or location.
  //
  // ⚠ IT STILL SAYS THE SENDER LEARNS YOU OPENED IT, because that remains true.
  // The fetch happens when the message is opened, so the timing is disclosed
  // either way; only caching changes that, and only from the second open. A line
  // that said "the sender learns nothing" would be the overstatement ADR-026's
  // Consequences warn about — describe what it DOES, not what it achieves.
  "thread.remoteBlockedProxied":
    "Remote images in this message were blocked. Loading them fetches them through your own server, so the sender still learns you opened it but not where you are or what you are reading with.",
  "thread.showRemote": "Show remote content",
  // Sender authentication (#55). Shown ONLY on the ~0.8% of mail with something
  // wrong — see core/senderAuth.ts for why there is deliberately no "verified"
  // counterpart, and why `none` is not reported here.
  //
  // ⚠ EACH LINE SAYS ONLY WHAT IS ACTUALLY KNOWN, because the three cases license
  // three different claims and the strongest one is not available for any of them.
  // None of these means "this is a forgery"; the honest ceiling is "the thing that
  // would normally rule that out did not happen", so the wording stops there and
  // says what the reader can do instead.
  "thread.auth.failed":
    "This message failed a sender check ({methods}), so it may not be from {sender}. Take care with links and attachments.",
  "thread.auth.unauthenticated":
    "Nothing vouched for this message's sender, so there is no way to tell whether it really came from {sender}.",
  "thread.auth.unverifiable":
    "A sender check could not be completed ({methods}), so it is unknown whether this message really came from {sender}.",
  // The method names, spelled out. The acronyms are meaningless to most readers and
  // the notice is aimed at people who do not know them.
  // Mailroom's own DKIM check (#776, ADR-036): shown in place of "unauthenticated"
  // when the server verified a signature itself. Names who vouched and for what.
  "thread.auth.verifiedByMailroom": "Verified by Mailroom (DKIM, {domain})",
  "thread.auth.verifiedByMailroomHint":
    "Your mail provider did not vouch for this message, so Mailroom checked its DKIM signature itself and it matched {domain}, the sender's domain.",
  "thread.auth.method.spf": "sender address",
  "thread.auth.method.dkim": "signature",
  "thread.auth.method.dmarc": "domain policy",
  // Sender-domain warnings (#397). A different question from thread.auth.* above:
  // not "did this authenticate" but "is the domain it came from a lookalike, a
  // punycode label, or a stranger claiming one of your own domains" — see
  // core/senderWarnings.ts. Shown alongside thread.auth.*, not instead of it, and
  // for the same reason: silence is the answer for the overwhelming majority of
  // mail, and each line says only what is actually known.
  //
  // selfSpoof is the most serious of the three: a message claiming one of the
  // reader's OWN domains that did not prove it.
  // Encrypted mail (#47 6b, ADR-037). ⚠ HONEST, NOT REASSURING: the server
  // holds the key and decrypts to show the message (ADR-024: not end-to-end
  // encrypted), so no string here may imply only the reader can see it. What is
  // true: it travelled encrypted, Mailroom decrypted it to show it, and the
  // decrypted text is not stored. Failure says no more than that it failed —
  // the server deliberately gives no reason (one opaque error, by design).
  "thread.encryption.decrypted":
    "Encrypted message. Mailroom decrypted it to show it to you; the decrypted text is not stored, so it does not appear in search.",
  "thread.encryption.failed": "This message is encrypted and could not be decrypted.",
  // PGP signatures (#47, ADR-039). Plain words, and never a colour alone. "Checks
  // out" is deliberately the weaker claim: the content matches a key Mailroom
  // holds for this sender, which is not proof of who they are - and the
  // changed-key wording says the one thing that matters about that out loud.
  "thread.signature.region": "Signature",
  "thread.signature.verified":
    "Signed, and the signature checks out against key {fingerprint}, the key you have for this sender.",
  "thread.signature.bad":
    "Signed, but the signature doesn't check out. The message may have been changed after it was signed.",
  // An expired signing key (#795): stale, not forged, and not verified either.
  "thread.signature.keyExpired": "Signed with a key that has expired.",
  "thread.signature.keyExpiredOn": "Signed with a key that expired on {date}.",
  "thread.signature.keyExpiredValidWhenSigned":
    "Signed with a key that expired on {date}; it was valid when this was signed.",
  "thread.signature.unknownKey":
    "Signed with a key you haven't seen from this sender, so the signature can't be checked.",
  "thread.signature.unknownKeyWith":
    "Signed with a key you haven't seen from this sender (key {fingerprint}), so the signature can't be checked.",
  "thread.signature.keyChanged":
    "Signed with a NEW key, different from the one you had for {address}.",
  "thread.signature.keyChangedRefused":
    "Signed with a different key from the one you had for {address}. You chose to keep the old key, so this one is not trusted.",
  "thread.signature.keyChangedWhy":
    "This can mean {address} changed keys, or that someone else is pretending to be them. Check with them another way before you trust it.",
  "thread.signature.before": "The key you had",
  "thread.signature.now": "The key on this message",
  "thread.signature.trust": "Trust this key from now on",
  "thread.signature.trustAnyway": "Trust this key after all",
  "thread.signature.keep": "Keep the old key",
  "thread.signature.compare": "Compare the keys",
  "thread.signature.compareHelp":
    "Ask {address} to read their key fingerprint to you by phone or in person, or compare it with one they published somewhere else. Don't use this message to check itself.",
  "thread.signature.deciding": "Saving your choice…",
  "thread.signature.importKey": "Import a key for this sender…",
  // Importing a correspondent's public key (#47, ADR-038). The dialog shows the
  // fingerprint and the user ID BEFORE the user confirms, and every refusal says in
  // one sentence what is wrong and what to do about it.
  "keyImport.title": "Import a key for {address}",
  "keyImport.intro":
    "Paste the public key {address} gave you, or choose the key file. Only a public key is accepted; a secret key is refused and not kept.",
  "keyImport.pasteLabel": "Public key",
  "keyImport.fileLabel": "Or choose a key file (.asc or .pgp)",
  "keyImport.check": "Check this key",
  "keyImport.checking": "Checking the key…",
  "keyImport.confirmHeading": "Is this the right key?",
  "keyImport.fingerprint": "Fingerprint",
  "keyImport.userId": "Belongs to",
  "keyImport.confirmHelp":
    "Compare the fingerprint with one {address} gave you another way: in person, by phone, or on a site you trust. Importing makes this the key Mailroom encrypts to for {address}, in place of any key it had.",
  "keyImport.import": "Import this key",
  "keyImport.importing": "Importing…",
  "keyImport.back": "Choose a different key",
  "keyImport.error.empty": "Paste a key or choose a file first.",
  "keyImport.error.secretKeyRefused":
    "That is a secret key. Mailroom did not keep it. Export the public key instead and import that.",
  "keyImport.error.keyDoesNotNameAddress":
    "That key doesn't name {address}, so it can't be used for them. Ask them for the key that does.",
  "keyImport.error.noEncryptionSubkey":
    "That key can't be used to encrypt mail: it has no encryption subkey. Ask for their full public key.",
  "keyImport.error.keyExpiredOrRevoked": "That key has expired or been revoked. Ask {address} for a current one.",
  "keyImport.error.invalidKey":
    "That isn't a single OpenPGP public key. Paste one key at a time, including the BEGIN and END lines.",
  "keyImport.error.tooLarge": "That key is too big to import. The limit is 256 KiB.",
  "keyImport.error.ownAddress": "That is one of your own addresses, so it isn't a contact's key.",
  "keyImport.error.unavailable": "Couldn't reach the server. Check your connection and try again.",
  "keyImport.error.failed": "Couldn't import that key. Try again.",
  "keyImport.error.fileUnreadable": "Couldn't read that file.",
  "thread.senderWarning.selfSpoof":
    "This message claims to be from {domain}, one of your own domains, but nothing about it proved that. Take care with links and attachments.",
  // lookalikeDomain names BOTH domains, because that is what makes it actionable
  // rather than merely ominous — the reader can check "is {domain} really
  // {resembles}?" in a second, which a bare "this looks suspicious" never lets
  // them do.
  // multipleFrom (#754): more than one From header. Said without a domain on
  // purpose — WHICH sender the message is "from" is the very thing in doubt.
  "thread.senderWarning.multipleFrom":
    "This message names more than one sender in its From header, so the sender shown may not be the one that was checked. Take care with links and attachments.",
  "thread.senderWarning.lookalikeDomain":
    "This message came from {domain}, which closely resembles {resembles} — a domain you correspond with — but is not it.",
  // punycodeDomain is deliberately neutral and informational, never alarming:
  // internationalized domains are ordinary and legitimate. It is worth a word
  // only because mail headers carry the punycode form, so what is shown here is
  // not what the domain renders as elsewhere.
  "thread.senderWarning.punycodeDomain":
    "This message's domain, {domain}, is written in an internationalized form that reads elsewhere as {unicode}.",
  // Same notice, for the rarer case where the punycode label did not decode to a
  // displayable form — still informational, still just the fact of the encoding.
  "thread.senderWarning.punycodeDomainOpaque":
    "This message's domain, {domain}, is written in an internationalized form.",
  // Remembering the decision (#202). The DOMAIN button spells out how broad it is
  // where the choice is made — a settings page nobody reads is not the place to
  // disclose that "anyone at example.com" includes senders you have never seen.
  "thread.alwaysAllowSender": "Always allow this sender",
  "thread.alwaysAllowDomain": "Always allow anyone at {domain}",
  "thread.alwaysAllowDomainHint":
    "Broader: this loads images from every sender at {domain}, including ones you have not seen yet.",
  // Shown when images loaded because of a remembered decision rather than a click.
  // A pane that silently behaves differently for one sender than another erodes
  // trust in the default, and this line is also the fastest route to undoing it.
  //
  // "Are shown", not "are loading" (#667): the line used to say they were loading
  // while every one of them failed. What did not arrive is said beside it, by
  // thread.imagesFailed below.
  "thread.remoteAllowedSender": "Images are shown because you always allow {sender}.",
  "thread.remoteAllowedDomain": "Images are shown because you always allow anyone at {domain}.",
  // The state of the images the user allowed (#667). The app fetches them itself
  // now, so it knows which arrived, and a banner that knows should say.
  "thread.imagesLoading": "Loading images…",
  "thread.imagesFailed.all": "The images could not be loaded.",
  "thread.imagesFailed.some": "{failed} of {total} images could not be loaded.",
  // Tracking pixels withheld (#55). Shown beside the lines above, because those
  // say images ARE loading and this is the part that is not — withholding some of
  // what the user allowed, without saying so, is the thing #202's line above
  // exists to avoid.
  //
  // ⚠ Singular and plural are separate entries rather than "{count} pixel(s)".
  // On a real mailbox the count is 1 or 2 on essentially every message
  // from an allowed sender, so the singular is the common case, not an edge one.
  //
  // "Tracking pixel" rather than "invisible image": it names what the thing is
  // FOR, which is the only part a reader can act on.
  "thread.pixelsBlocked.one": "1 tracking pixel was blocked.",
  "thread.pixelsBlocked.many": "{count} tracking pixels were blocked.",
  "thread.stopAllowing": "Stop allowing",

  // Attachment preview (#131). The point of the wording is that looking is the
  // SAFER option, not the lazier one: the alternative is downloading a stranger's
  // document and opening it in a desktop application.
  "attachment.previewTitle": "Preview of {name}",
  "attachment.download": "Download",
  "attachment.downloadNamed": "Download {name}",
  "attachment.previous": "Previous attachment",
  "attachment.next": "Next attachment",
  "attachment.counter": "{n} of {total}",
  "attachment.loading": "Opening preview…",
  // Shown when the server would not serve this part as a type we are willing to
  // render — which is exactly what happens to an HTML or SVG part, by design.
  "attachment.noPreview": "No preview for this file type — download it to open it.",
  // A download or preview the server refused, explained in the app instead of
  // as the browser's bare 403 page. The first word is the meaning ("Blocked"),
  // so nothing depends on colour; flagged and unknown stay different sentences
  // (ADR-027 section 5b).
  "attachment.blocked.flagged": "Blocked: the virus scanner flagged this attachment.",
  "attachment.blocked.flaggedNamed":
    "Blocked: the virus scanner flagged this attachment ({detection}).",
  "attachment.blocked.unknown":
    "Blocked: this attachment was not scanned because the scanner could not check it.",
  "attachment.fetchFailed": "This attachment could not be fetched right now.",
  "attachment.tooBig": "Too large to preview — download it instead.",
  "attachment.truncated": "Preview shortened — download the file to see all of it.",

  // The malware-quarantine chip (#396/#412). A withheld part is NEITHER a
  // preview link nor a download link — both would 403 — so it gets its own
  // shape: filename, size, a plain statement that it is withheld, WHY (in one
  // of two different sentences, because "flagged" and "unknown" are different
  // findings and conflating them would claim the scanner caught something it
  // never actually looked at), and the Release control.
  "attachment.withheldState": "Withheld",
  "attachment.withheldLabel": "{name} — withheld",
  "attachment.withheldReason.flagged": "The scanner flagged this file as malware.",
  "attachment.withheldReason.unknown":
    "The scanner could not check this file — often because it is encrypted or password-protected.",
  // Only reached for a scan state `attachscan.Blocked` does not actually admit
  // to `quarantined` today (see attachmentScanPolicy.ts) — kept honest and
  // generic rather than silent.
  "attachment.withheldReason.generic": "The scanner withheld this file.",
  "attachment.release": "Release",
  "attachment.releasing": "Releasing…",
  // ⚠ THE CONFIRMATION MUST NOT REASSURE. This is a security control, not a
  // "delete this?" prompt — it exists to make a real detection hard to click
  // through on muscle memory, so the wording states the consequence rather than
  // asking "are you sure?".
  "attachment.releaseConfirmTitle": "Release {name}?",
  // ⚠ "MAILROOM WILL NOT CHECK IT AGAIN" WOULD BE FALSE, and it was the first
  // draft of this string. A release does NOT stop scanning: the verdict lives in
  // app.attachments and keeps being refreshed on every re-scan pass, while the
  // release is a separate row keyed on the file's content
  // (internal/store/attachments.go — "the verdict stays `flagged` and keeps
  // being refreshed, and the gate stays open"). What a release overrides is the
  // WITHHOLDING, not the checking. Saying otherwise would tell the user the
  // scanner has stopped looking, which is both untrue and the more alarming of
  // the two readings.
  // ⚠ "UNLESS YOU UNDO THIS" WAS ALSO FALSE (#626 §A). core/actions.ts's
  // `releaseAttachment` takes a `revoke` option and internal/store can re-arm the
  // gate, but nothing in this web client ever calls it that way — none of the
  // three release sites (ThreadPane.tsx, viewer/HeldPanel.tsx,
  // compose/AttachmentTiles.tsx) passes `{ revoke: true }`. There is no undo
  // button here, so the sentence must not promise one.
  "attachment.releaseConfirmBody":
    "Releasing this file lets it be downloaded and attached to outgoing mail, permanently — Mailroom keeps checking it, but this override does not come with an undo here. Only do this if you know what the file is and where it came from.",
  "attachment.releaseAnyway": "Release anyway",
  "attachment.keepWithheld": "Keep it withheld",

  // THE ATTACHMENT VIEWER (#449, design pass-2 §9). The lightbox's words, plus the
  // ones a surface that takes the main area needs: what the file IS, what you can
  // do with it, and — the part this catalogue carries the most weight for — what a
  // HELD file says instead of showing itself.
  "viewer.title": "Attachment: {name}",
  // The tooltip on the tile in the reading pane. It says what clicking DOES —
  // "Preview of X" was the lightbox's word for it and the viewer is more than a
  // preview now (it is also where a held file is explained and released).
  "viewer.open": "Open {name}",
  // `from <Sender>` in the top bar. The sender is a fact about the FILE here: the
  // question a reader has in front of an unexpected invoice is who sent it.
  "viewer.from": "from {sender}",
  "viewer.forward": "Forward",
  "viewer.openInTab": "Open in a tab",
  // The words on the size controls. `100%` rather than "Actual size": it is the
  // shorter of the two and it is what every image viewer calls it.
  "viewer.zoom": "Size",
  "viewer.fit": "Fit",
  "viewer.actualSize": "100%",
  "viewer.fitWidth": "Fit width",
  // An image's intrinsic size, once decoded. `×` is the multiplication sign, not
  // the letter x.
  "viewer.dimensions": "{w} × {h}",
  "viewer.strip": "Files on this message",
  // One strip item: the name and its kind word. `held` is a THIRD clause rather
  // than a different sentence, so the two forms differ only by what is added —
  // colour and a dashed border reach nobody who is listening.
  // Kind FIRST, as on screen: the thumbnail prints the type glyph and then the
  // file name, and the name must contain that text in that order (WCAG 2.5.3,
  // axe label-content-name-mismatch from axe-core 4.14).
  "viewer.strip.item": "{kind} · {name}",
  "viewer.strip.itemHeld": "{kind} · {name} · held",
  // The kind word for the bucket every dangerous type is downgraded into
  // (application/octet-stream) and for a part with no type at all.
  "viewer.kind.file": "file",
  // ⚠ "YET". Office and ODF documents are the common case here and #199 is the
  // issue that renders them; a flat "No preview for this type" would read as a
  // decision rather than as a thing not built.
  "viewer.noPreview": "No preview for this type yet",
  "viewer.noPreviewNote": "Download it to open it in an application on your device.",
  // The mark a release leaves, in the viewer's top bar and on the tile in the
  // reading pane. It outlives the hold on purpose: it is a fact about the user's
  // own decision, and the server keeps reporting it even when a later re-scan
  // comes back clean.
  "viewer.released": "released by you",
  "viewer.releasedOn": "released by you · {date}",

  // ── A HELD FILE OPENS TO AN EXPLANATION (ADR-027 §8) ─────────────────────
  //
  // TWO CASES, TWO WEIGHTS, ONE VOCABULARY. Both say that a release covers every
  // copy of the same file in the mailbox — which is what the server does, because
  // `attachment_releases` is keyed by the part's content hash — and neither invents
  // a reason the scanner did not give.
  //
  // ⚠ THE UNCHECKED CASE NEVER ACCUSES. "Could not be scanned" is the scanner
  // failing to reach a verdict, most often on a password-protected archive; a
  // sentence that hedged toward "might be dangerous" would tell the user something
  // nothing has established, about a file that is usually their own payslip.
  "viewer.held.unscanned.title": "This file is held: it could not be scanned.",
  "viewer.held.unscanned.body":
    "The scanner could not open it — usually a password-protected archive or PDF. That is not a verdict about the file; it means nothing has checked it. Held files cannot be opened, saved or forwarded. Releasing lets you do all three, and covers every copy of this same file in your mailbox, now and later.",
  "viewer.held.unscanned.release": "Release and open",
  // ⚠ AND THE FLAGGED CASE NEVER SOFTENS. This is a detection, the risk lands on
  // the device the file gets opened on, and the sentence says so — the button that
  // agrees to it is the secondary one and it asks again.
  "viewer.held.flagged.title": "This file is flagged by the scanner.",
  "viewer.held.flagged.body":
    "Flagged files are held because the scanner believes they are harmful, not because it could not read them. Releasing a flagged file is a real risk to the device you open it on. If you release it anyway, it is released everywhere it appears and marked as released by you, with the date.",
  "viewer.held.flagged.release": "I understand the risk — release",
  "viewer.held.keep": "Keep held",
  // The second ask, for a flagged file only. It states what will be true
  // afterwards rather than asking "are you sure?" — the same rule
  // attachment.releaseConfirmBody follows, and for the same reason: a yes/no
  // question invites a reflexive yes.
  "viewer.held.confirmTitle": "Release {name} anyway?",
  "viewer.held.confirmBody":
    "The scanner flagged this file as harmful. Releasing it lets it be downloaded and attached to outgoing mail — every copy of the same file, now and later — and records that you released it, with the date. Only do this if you are certain what the file is.",
  "viewer.held.confirmRelease": "Release it anyway",

  "compose.title": "New message",
  "compose.replyTitle": "Reply",
  "compose.replyAllTitle": "Reply all",
  "compose.from": "From",
  "compose.to": "To",
  "compose.cc": "Cc",
  "compose.subject": "Subject",
  "compose.body": "Message",
  "compose.identityHint":
    "Your From address must be a verified identity — the server rejects anything else.",
  "compose.wildcardHint":
    "{domain} is a catchall: you can send as any address at this domain.",
  "compose.quoteHeader": "On {date}, {who} wrote:",
  "compose.attach": "Attach files",
  "compose.attachments": "Attached files",
  "compose.uploading": "Uploading…",
  "compose.attachFailed": "Could not attach {name}: {error}",
  "compose.removeAttachment": "Remove {name}",
  "compose.sending": "Sending…",
  "compose.sendShortcut": "Send (Ctrl+Enter, ⌘+Enter on a Mac)",
  // ENCRYPT (#47, ADR-038). The switch is offered only when every To/Cc
  // recipient has a usable key; the sentences below say why it is not.
  "compose.encrypt": "Encrypt",
  "compose.encryptOn": "Encrypted to {names} and to you. The subject and recipients stay readable.",
  "compose.encryptBcc":
    "Can't encrypt with a Bcc recipient: one encrypted message would reveal every recipient's key, including the Bcc ones.",
  "compose.encryptMissing": "Can't encrypt: no key for {names}.",
  "compose.encryptChanged":
    "Can't encrypt: the key for {names} has changed since you last wrote to them, and has to be confirmed first.",
  "compose.encryptChangedHeading":
    "The key for {address} is different from the one you last wrote to.",
  "compose.encryptPending": "Checking for encryption keys…",
  // Recipients' key dates (#795). Said by name, so the user knows whom to ask.
  "compose.keyExpiresOn": "{name}'s key expires on {date}.",
  "compose.keyExpiredOn": "Can't encrypt: {name}'s key expired on {date}.",
  "compose.keyExpired": "Can't encrypt: {name}'s key has expired.",
  "compose.keyRevoked": "Can't encrypt: {name}'s key was revoked.",
  "compose.refreshKey": "Check for an updated key",
  "compose.refreshKeyFor": "Check for an updated key for {name}",
  "compose.refreshNoKey": "No key is held for {name} to check.",
  "compose.importKey": "Import their key…",
  "compose.importKeyFor": "Import their key… ({address})",
  "compose.sendArchiveShortcut": "Send & archive (Ctrl+Shift+Enter, ⌘+Shift+Enter on a Mac)",
  // SELECTIVE SYNC (#62). The wording avoids "off"/"disabled" on purpose: nothing
  // was deleted and nothing is broken, the folder is simply no longer being
  // updated. "Paused" says that; "off" invites the reading that the mail is gone.
  "sync.frozenTag": "paused",
  "sync.frozenHint":
    "This folder is not being synced. What you see is what was here when you paused it — new mail will not appear, and mail deleted elsewhere will still be listed.",
  "sync.pauseFolder": "Stop syncing {name}",
  "sync.resumeFolder": "Resume syncing {name}",
  "sync.pauseHint": "Stop syncing this folder. Nothing is deleted; it stops being updated.",
  "sync.resumeHint": "Resume syncing this folder and catch up on what changed.",
  "compose.sent": "Message sent.",
  // Send & archive (#648). NOT "sent and archived": the message is still in its
  // undo window and the server archives the conversation only once it has
  // actually gone — a send that fails or is undone leaves it where it is.
  "compose.sentWillArchive": "Message sent. The conversation will be archived once it has gone.",
  // OFFLINE QUEUED SEND (#62). Deliberately not "Message sent." and not an error:
  // the message is safe and it has not gone anywhere. Saying which of those is true
  // is the whole point — see core/actions.ts SendOutcome.
  "compose.queued": "You're offline — this message is saved and will send when you reconnect.",
  "compose.queuedWillArchive":
    "You're offline — this message is saved and will send when you reconnect. The conversation will be archived once it has gone.",
  "compose.queuedDraft": "You're offline — this draft is saved and will sync when you reconnect.",
  "compose.needRecipient": "Add at least one recipient.",
  "compose.needFrom": "Choose the address to send from.",
  "compose.formatting": "Text formatting",
  "compose.bold": "Bold",
  "compose.italic": "Italic",
  "compose.underline": "Underline",
  "compose.bulletList": "Bulleted list",
  "compose.numberList": "Numbered list",
  "compose.link": "Insert link",
  "compose.linkPrompt": "Link address (https://…)",
  "compose.insertImage": "Insert image",
  "compose.richMode": "Rich text",
  "compose.plainMode": "Plain text",
  "compose.dropHint": "Drop files here to attach",
  "compose.aliasHint": "Sending from {alias} — the address this contact reached you at.",

  // ---- compose reshaped (#448, design pass-2 §7) ---------------------------
  //
  // THE FROM IS A SENTENCE, so each reason exists twice: once as the clause under
  // the address (`compose.from.*`) and once as the ONE WORD a row of the Change
  // list prints (`compose.from.reason.*`). They are not the same string with
  // different lengths — the sentence explains, the word labels — and sharing one
  // would make every row of the list a paragraph.
  "compose.forwardTitle": "Forward",
  "compose.draftTitle": "Draft",
  "compose.bcc": "Bcc",
  "compose.addCc": "Add Cc",
  "compose.addBcc": "Add Bcc",
  "compose.from.becauseReplying": "because you are replying to mail sent there",
  "compose.from.primary": "your primary address",
  "compose.from.catchAll": "a catch-all address at this domain",
  "compose.from.disabled": "this address cannot send",
  "compose.from.none": "no sending address yet",
  "compose.from.change": "Change",
  "compose.from.pick": "Send from",
  "compose.from.filter": "Filter addresses",
  "compose.from.noMatch": "No address matches that.",
  "compose.from.localPart": "Address at this domain",
  "compose.from.reason.replyingHere": "replying here",
  "compose.from.reason.primary": "primary",
  "compose.from.reason.catchAll": "catch-all",
  "compose.from.reason.disabled": "disabled",
  // The note under the body when the message goes out as a non-primary alias. It
  // states the thing a reader of the SENT message would otherwise have to work
  // out — that answering it comes back to the alias, not to the main address.
  "compose.aliasNote":
    "Sending from your {alias} address. Replies come back to the same place.",

  // DISCLOSURE (#533, spec §5) — a different statement from aliasNote above, and
  // deliberately rare: it fires only for a recipient who was not already on this
  // thread, i.e. a forward or someone added to a reply. On a plain reply the
  // correspondent is the one who used the alias, and saying so again on every
  // reply would be the warning nobody reads.
  //
  // It says what the address REVEALS, not "are you sure": the operator invented
  // this scheme on purpose, and the useful fact is that per-correspondent
  // addresses make the rest of them guessable.
  "compose.aliasDisclosure.one":
    "{who} has not seen {alias} before — it shows how your addresses are made.",
  "compose.aliasDisclosure.other":
    "{n} recipients have not seen {alias} before ({who}) — it shows how your addresses are made.",

  // The chips' key fact. `unknown` prints NOTHING at all, so it has no string:
  // "no key" for a correspondent nobody could ask about is a claim nobody made.
  "compose.chip.keyHeld": "key held",
  "compose.chip.noKey": "no key",
  "compose.chip.keyExpired": "key expired",
  "compose.chip.keyRevoked": "key revoked",
  "compose.chip.remove": "Remove {name}",

  // THE SEND LINE. It never promises to encrypt — encryption is v1.69.0 — so the
  // sealed sentence names who COULD be encrypted to, and the warned one says
  // outright that the message goes in the clear. Never a silent downgrade.
  "compose.send.willSign": "Will sign",
  "compose.send.keysHeldFor": "keys held for {names}",
  "compose.send.noKeyFor": "no key for {names}",
  "compose.send.plainToAll.one": "would send as plain text",
  "compose.send.plainToAll.other": "would send as plain text to all {count}",
  // Branch 3's two reasons (#492) — WHY there is no signature, not just that
  // there is none. "noKey" names the address with nothing bound to it; "cannotSign"
  // is server-wide and names no address, because binding a key would not have
  // mattered on a server with no crypto process wired at all.
  "compose.send.willNotSign.noKey": "Will not sign: no key bound to {address}",
  "compose.send.willNotSign.cannotSign": "Will not sign: this server cannot sign",
  "compose.send.unknownServer": "Signing status unknown on this server",

  // Attachments. The drop zone is one line that says both what the control does
  // and what the surface accepts.
  "compose.attachZone": "Attach · or drop files here",
  "compose.attachFromMessage": "from the message",
  "compose.attachHeld": "Held · cannot be forwarded until released",
  "compose.attachRelease": "Release…",
  "compose.insertTemplate": "Insert template",
  // Any saved signature (#649) — not just the one auto-inserted for the From
  // identity. See ui/compose/signature.ts for the replace-or-insert rule.
  "compose.insertSignature": "Insert signature",

  // Autosave, in the footer rather than as a toast: it is a fact about this panel,
  // and a toast every fifteen seconds behind a refusing server would be a machine
  // gun. "Not saved yet" is the honest opening state — nothing has been saved and
  // nothing has failed.
  "compose.saved": "Saved a moment ago",
  "compose.saving": "Saving…",
  "compose.notSavedYet": "Not saved yet",
  "compose.notSaved": "Not saved · {reason}",
  "compose.discard": "Discard",
  // The composer's column while the message it is about is still being fetched
  // (a forward, or a resumed draft, reached by a deep link). Not an empty
  // composer: typing into one that is about to be replaced would lose the text.
  "compose.loading": "Opening…",
  // The composer's column when the server says the message it was opened for
  // does not exist (#612). Sent, discarded, or replaced by a later save from
  // another tab — this composer cannot tell which, so it names all three and
  // says where the newest version would be.
  "compose.missing":
    "This draft is no longer here. It may have been sent, discarded, or replaced by a newer save — the newest version is in Drafts.",
  // The same, for a forward whose original is gone: there is nothing to quote
  // or re-attach, and nothing of the user's was lost — they had not begun.
  "compose.missingForward": "The message you were forwarding is no longer here.",
  // Closes the composer's column while it has no composer in it — waiting, or
  // gone. Without it the only way out of a composer that never opened was
  // Escape, which nothing on screen mentions, and the list stayed squeezed into
  // the composer's layout until the user found it (#612).
  "compose.close": "Close",

  // Add-account wizard (spec §12): add-account form → server validates + detects
  // folders → initial-sync progress. Credentials are sealed server-side; the
  // password is write-only and never shown again.
  "account.add": "Add mail account",
  "account.add.title": "Add a mail account",
  "account.add.open": "Add mail account…",
  "account.name": "Account name",
  "account.name.hint": "A label for this account, e.g. “Work” or “Personal”.",
  "account.imapHost": "IMAP server",
  "account.imapPort": "IMAP port",
  "account.smtpHost": "SMTP server",
  "account.smtpPort": "SMTP port",
  "account.username": "Username",
  "account.password": "Password",
  // ⚠ A CONNECTION CHANGE IS ALL-OR-NOTHING (#374). The server classifies a patch
  // touching any endpoint or credential field as a connection change, re-validates
  // the WHOLE login against IMAP, and never opens the stored password to reuse it —
  // so "leave the password alone" is not a state it can be asked for.
  "account.credentials.mustReenter":
    "Changing any server setting re-checks the whole login, so the username and password have to be typed again. The server never sends the stored password back, so it cannot reuse it.",
  "account.password.hint":
    "Sent once and stored encrypted on the server — it is never shown again. Use an app password if your provider offers one.",
  "account.advanced": "Server settings",
  "account.submit": "Add account",
  "account.validating": "Connecting to your mail server and detecting folders…",
  "account.added": "“{name}” is set up. Initial sync has started — your mail will appear as it downloads.",
  "account.needName": "Give the account a name.",
  "account.needImapHost": "Enter the IMAP server address.",
  "account.needSmtpHost": "Enter the SMTP server address.",
  "account.needUsername": "Enter the account username.",
  "account.needPassword": "Enter the account password.",
  "account.validationFailed":
    "Could not connect and sign in to that mail server. Check the server addresses, username, and password.",
  "account.error": "Could not add the account: {error}",

  // Account-health panel (spec §12, mailroom#43): a read-only, on-demand view of
  // each account's sync status, delivery failures, stuck changes, and send quota.
  "health.open": "Account health",
  "health.title": "Account health",
  "health.loading": "Checking your accounts…",
  "health.error": "Could not load account health: {error}",
  "health.empty": "No mail accounts yet.",
  "health.statusOk": "Healthy",
  "health.statusWarn": "Needs attention",
  "health.statusError": "Not syncing",
  "health.sync": "Sync",
  "health.syncedAgo": "Synced {ago}",
  "health.neverSynced": "Never synced yet.",
  "health.syncError": "Last sync failed: {error}",
  "health.consecutiveFailures": "{count} failed attempts in a row.",
  "health.delivery": "Delivery failures",
  "health.deliveryNone": "No recent delivery failures.",
  "health.deliveryCount": "{count} recent delivery failures:",
  "health.deadLetters": "Stuck changes",
  "health.deadLettersNone": "No stuck changes.",
  "health.deadLettersCount": "{count} changes could not be applied to the server:",
  "health.sendQuota": "Sends today",
  "health.sendQuotaMeter": "{sent} of {limit} sends used today",
  // Search staleness (spec §7, mailroom#41): one per-USER line, shown only when
  // the server has a search index installed — absent means the field itself was
  // omitted from Account/getHealth, not that it is stale.
  "health.searchUpToDate": "Search up to date",
  "health.searchStale": "Search may be up to {minutes} minutes stale",

  // Ingest-only mode (ADR-028, #521): the EFFECTIVE writeback flag, which
  // already ANDs the account's own column with an instance-level switch
  // (ADR-028 §2) — this is what actually happened to the account's last
  // write, not merely what the settings toggle is set to.
  "health.writeback": "Writeback",
  "health.writeback.on": "Mailroom syncs and writes to this account normally.",
  "health.writeback.off": "Ingest only: Mailroom is not writing to this account.",
  "health.writeback.flagsProvisional":
    "Read/unread and stars are provisional — they mirror the server and will be replaced by it.",
  "health.writeback.localDurable": "Labels, filing and trash stay local and are durable.",
  "health.writeback.suppressed.one": "1 change recorded but not sent.",
  "health.writeback.suppressed.other": "{count} changes recorded but not sent.",

  "labels.title": "Apply a label",
  "labels.none": "You have no labels yet.",
  "labels.create": "Create label",
  "labels.name": "Label name",

  "palette.title": "Command palette",
  "palette.placeholder": "Type a command…",
  "palette.noResults": "No matching command.",

  "settings.title": "Settings",
  // ONE SEARCHABLE PAGE OF TWELVE ROWS (#374 increment 2). The row NAMES are what
  // a user would call the thing, not what the code calls it: "The line" and
  // "In-tray" are the desk's own words, and "Reading and safety" is the question
  // ("what does opening a message expose me to?") rather than the mechanism.
  "settings.appearance": "Appearance",
  "settings.accounts": "Accounts",
  "settings.areas": "Areas and drawers",
  "settings.line": "The line",
  "settings.tray": "In-tray",
  "settings.identities": "Identities and signatures",
  "settings.reading": "Reading and safety",
  "settings.keys": "Keys",
  "settings.data": "Data",
  "settings.signOut": "Sign out",
  // The one disclosure a row is allowed, for its rarer controls.
  "settings.more": "more ▸",
  "settings.less": "less ▾",
  "settings.changed": "changed",
  "settings.reset": "Reset",

  // WHO A SETTING APPLIES TO. Four short words with a sentence each behind them:
  // the words fit on a row, and the sentences are what stop a four-word
  // vocabulary from being guessed at wrongly.
  "settings.scope.login": "login",
  "settings.scope.login.explain":
    "Applies to your whole login — every account, every device, every browser.",
  "settings.scope.account": "account",
  "settings.scope.account.explain": "Applies to one mail account.",
  "settings.scope.area": "area",
  "settings.scope.area.explain": "Applies to one area of your desk.",
  "settings.scope.web": "web",
  "settings.scope.web.explain":
    "Applies to this web app — synced between the browsers you sign in from, not to a phone app.",

  // The search field. The label is visible rather than a placeholder, because a
  // placeholder disappears the moment anyone types.
  "settings.search.label": "Find a setting",
  "settings.search.placeholder": "Type to find a setting or its value…",
  "settings.search.none": "Nothing here matches “{query}”.",

  // THE STATE LINES. Every one of these is a FACT the page has actually measured;
  // a clause with no measurement behind it is left out rather than printed as a
  // zero (ui/routes/SettingsRoute.tsx says why at each builder).
  "settings.state.acctOk": "ok",
  "settings.state.acctPaused": "paused",
  "settings.state.acctFailing": "not syncing",
  "settings.state.areas.one": "1 area",
  "settings.state.areas.other": "{count} areas",
  "settings.state.labelsNoArea.one": "1 label in no area",
  "settings.state.labelsNoArea.other": "{count} labels in no area",
  "settings.state.needSorting.one": "1 needs sorting",
  "settings.state.needSorting.other": "{count} need sorting",
  "settings.state.drawersSweep.one": "1 drawer auto-archives",
  "settings.state.drawersSweep.other": "{count} drawers auto-archive",
  // The login-level master switch is off, so nothing is swept whatever the areas
  // say (types.ts DeskPreferences.drawerSweepEnabled).
  "settings.state.sweepOff.one": "1 drawer is set to auto-archive, but sweeping is off",
  "settings.state.sweepOff.other": "{count} drawers are set to auto-archive, but sweeping is off",
  "settings.state.threshold": "threshold {value}",
  "settings.state.trayNag.one": "nag after 1 day",
  "settings.state.trayNag.other": "nag after {count} days",
  "settings.state.trayNotRouted": "returned snoozes and bounces are not yet routed here",
  "settings.state.rulesSuggested": "{count} suggested",
  "settings.state.identitiesAcross": "{identities} across {accounts}",
  "settings.state.identityCount.one": "1 identity",
  "settings.state.identityCount.other": "{count} identities",
  "settings.state.accountCount.one": "1 account",
  "settings.state.accountCount.other": "{count} accounts",
  "settings.state.signatures.one": "1 signature",
  "settings.state.signatures.other": "{count} signatures",
  "settings.state.templates.one": "1 template",
  "settings.state.templates.other": "{count} templates",
  "settings.state.pushOn": "push on",
  "settings.state.pushOff": "push off",
  "settings.state.pushBlocked": "blocked",
  "settings.state.pushUnavailable": "push is not offered by this server",
  "settings.state.pushUnsupported": "this browser cannot show push",
  "settings.state.quietOff": "quiet hours off",
  "settings.state.quietOn": "quiet {start}–{end}",
  "settings.state.remoteImages": "remote images: ask each time",
  "settings.state.sendersAllowed.one": "1 sender always allowed",
  "settings.state.sendersAllowed.other": "{count} senders always allowed",
  "settings.state.theme": "theme {value}",
  "settings.state.density": "density {value}",
  // Only rendered when they are NOT the default — see appearanceStateText.
  "settings.state.textSize": "text {value}",
  "settings.state.rows": "rows {value}",
  "settings.state.opensBeside": "message opens beside the list",
  "settings.state.opensFocused": "message opens on its own",
  "settings.state.dataOperator": "export and deletion are run by your operator",

  // The SERVER's density words (relaxed / normal / compact), which are not this
  // app's own (comfortable / cozy / compact). The state line prints these because
  // it is naming the value as it is SYNCED — the word another device shows for
  // the same setting. app/prefs.ts owns the mapping between the two vocabularies.
  "settings.serverDensity.relaxed": "relaxed",
  "settings.serverDensity.normal": "normal",
  "settings.serverDensity.compact": "compact",

  // The reading-pane divider: a readout and a reset, not a second control for a
  // value the divider itself already sets.
  "settings.split": "Pane split",
  "settings.split.value": "{list} / {message}",
  "settings.split.unknown": "Not synced yet.",
  "settings.split.hint":
    "Drag the divider on the desk to change · synced with your other browsers",
  "settings.appearance.scopeNote":
    "Scope: web. Synced across every browser and installed web app you sign in from. Where you left off (account scope, tab, scroll) stays per device.",
  "settings.paneLayout.deviceOnly":
    "This one stays with this device: a width that reads well on a large screen is not a height on a laptop.",

  // Identities as a PRECEDENCE — which address a reply actually goes out as.
  "settings.identities.precedence": "Which address mail goes out as",
  "settings.identities.explain":
    "In order. A concrete address you can send from comes before a catch-all that only accepts; anything switched off cannot send at all.",
  "settings.identities.none": "No identities have synced yet.",
  "settings.identities.reason.primary": "primary",
  "settings.identities.reason.catch-all": "catch-all",
  "settings.identities.reason.disabled": "disabled",

  // KEYS. Four states, and only three of them are about keys — a server that
  // cannot be asked has not told you that you have none.
  "settings.keys.unavailable": "keys are not available on this server",
  "settings.keys.unavailableLong":
    "This server does not offer the keys API, so Mailroom cannot tell you anything about your OpenPGP key or the keys it holds for your correspondents. None of that is a statement that you have no key.",
  "settings.keys.asking": "Asking the server…",
  "settings.keys.explain":
    "Your OpenPGP key, and how many of the people you correspond with have one on file here.",
  "settings.keys.none": "no key",
  "settings.keys.noneLong": "You have no OpenPGP key on this server.",
  "settings.keys.onePublished": "1 key, published",
  "settings.keys.oneUnpublished": "1 key, not published",
  "settings.keys.many": "{count} keys, {published} published",
  "settings.keys.published": "published",
  "settings.keys.notPublished": "not published",
  "settings.keys.created": "created {date}",
  // Which identity a key signs FOR (#492) — Identity.signingKeyId read
  // backwards, from state.identities. Beside the key it names, not a separate
  // fact line: it is the key's own binding, not a property of the account.
  "settings.keys.signsAs": "signs as {address}",
  "settings.keys.correspondents.one": "1 correspondent key held",
  "settings.keys.correspondents.other": "{count} correspondent keys held",
  "settings.keys.correspondentsLong": "Keys held for {count} of your correspondents.",
  "settings.keys.correspondentsUnknown":
    "This server did not say how many correspondent keys it holds.",
  "settings.keys.factExpired": "a key has expired",
  "settings.keys.factExpires": "a key expires {date}",

  // KEY MANAGEMENT (#796) and expiry (#795). Plain words: what each verb does,
  // what it costs, and whether it can be undone. No raw codes, ever.
  "keys.yours.heading": "Your keys",
  "keys.yours.explain":
    "Your OpenPGP keys sign the mail you send and let people encrypt mail to you. This server keeps them and uses them for you.",
  "keys.yours.warnWindow": "You're warned {days} days before a key expires.",
  "keys.yours.noCustody":
    "This server can't make, import, extend, back up or revoke keys (no key service is connected). You can still choose a key's address, publish it, or retire it.",
  "keys.fingerprint": "Fingerprint",
  "keys.keyFor": "Key for {address}",
  "keys.keyUnbound": "Key not used for any address",
  "keys.created": "Created {date}",
  "keys.expires": "Expires {date}",
  "keys.neverExpires": "Never expires",
  "keys.expiredOn": "Expired on {date}",
  "keys.expiredNoDate": "Expired",
  "keys.revokedOn": "Revoked on {date}",
  "keys.chips": "Status",
  "keys.chip.signsAs": "Signs as {address}",
  "keys.chip.published": "Published",
  "keys.chip.expiringSoon": "Expires soon",
  "keys.chip.expired": "Expired",
  "keys.chip.retired": "Retired",
  "keys.chip.revoked": "Revoked",
  "keys.chip.pinned": "Used for {addresses}",
  "keys.chip.refused": "Refused by you",
  "keys.chip.noEncryption": "Can't be encrypted to",
  "keys.backupHint":
    "This server made this key and holds the only copy. Back it up, so you can still read your mail if the server is lost.",
  "keys.actionsFor": "Actions for {key}",
  "keys.bind.label": "Use this key for",
  "keys.bind.none": "No address",
  "keys.bind.save": "Save",
  "keys.publish.publish": "Publish in the key directory",
  "keys.publish.withdraw": "Withdraw from the key directory",
  "keys.publish.at": "People's mail apps find it at",
  "keys.publish.willBeAt": "Once published, people's mail apps find it at",
  "keys.publish.wkdOff": "This server doesn't publish keys (its key directory, WKD, isn't set up).",
  "keys.publish.notBound": "Choose an address for this key before publishing it.",
  "keys.publish.notServable": "This server publishes keys only for addresses at {domain}.",
  "keys.publish.retired": "A retired or revoked key can't be published.",
  "keys.publish.unknown": "This server didn't say whether it publishes keys.",
  "keys.action.extend": "Extend…",
  "keys.action.backup": "Back up…",
  "keys.action.retire": "Retire…",
  "keys.action.revoke": "Revoke…",
  "keys.extend.title": "Extend this key",
  "keys.extend.titleFor": "Extend the key for {address}",
  "keys.extend.explain":
    "The key keeps its fingerprint, so nobody sees a new key. People who already have it pick up the new date the next time they fetch it.",
  "keys.extend.legend": "New expiry",
  "keys.extend.oneYear": "1 year from today",
  "keys.extend.twoYears": "2 years from today",
  "keys.extend.custom": "A number of days",
  "keys.extend.customLabel": "Days from today (1 to {max})",
  "keys.extend.customInvalid": "Enter a whole number of days from 1 to {max}.",
  "keys.extend.submit": "Extend",
  "keys.extend.busy": "Extending the key…",
  "keys.backup.title": "Back up this key",
  "keys.backup.titleFor": "Back up the key for {address}",
  "keys.backup.explain":
    "The file is protected by a passphrase you choose now. You need the passphrase to import the key again, in Mailroom or another mail app.",
  "keys.backup.notKept": "Mailroom keeps neither the passphrase nor the file.",
  "keys.backup.passphrase": "Passphrase (at least {min} characters)",
  "keys.backup.again": "Type the passphrase again",
  "keys.backup.tooShort": "Use at least {min} characters.",
  "keys.backup.mismatch": "The two passphrases don't match.",
  "keys.backup.submit": "Download backup",
  "keys.backup.busy": "Preparing the file…",
  "keys.retire.title": "Retire this key?",
  "keys.retire.what":
    "Retiring stops this key signing your mail and takes it out of the key directory. Mail already encrypted to it can still be opened.",
  "keys.retire.unbinds": "It stops being the key for {address}.",
  "keys.retire.permanent":
    "This can't be undone. To sign again, make or import another key. Retiring tells nobody else; to warn people off a stolen key, revoke it.",
  "keys.retire.submit": "Retire key",
  "keys.retire.busy": "Retiring…",
  "keys.revoke.title": "Revoke this key?",
  "keys.revoke.titleFor": "Revoke the key for {address}?",
  "keys.revoke.what":
    "Revoking tells everyone who fetches this key that it must not be used any more. Use it if the key may have been stolen, or you've replaced it.",
  "keys.revoke.permanent": "This can't be undone.",
  "keys.revoke.stillDecrypts": "Mail already encrypted to this key can still be opened here.",
  "keys.revoke.reasonLegend": "Why are you revoking it?",
  "keys.revoke.reason.compromised": "It may have been stolen or copied",
  "keys.revoke.reason.superseded": "I've replaced it with a new key",
  "keys.revoke.reason.retired": "I've stopped using it",
  "keys.revoke.confirmLabel": "To confirm, type the last 8 characters of the fingerprint ({tail})",
  "keys.revoke.waitingReason": "Choose a reason, then type the characters, to revoke.",
  "keys.revoke.waitingTail": "Revoke key becomes available once the characters match.",
  "keys.revoke.ready": "Ready. Revoke key can't be undone.",
  "keys.revoke.submit": "Revoke key",
  "keys.revoke.busy": "Revoking…",
  "keys.generate.open": "Make a new key…",
  "keys.generate.title": "Make a new key",
  "keys.generate.explain": "The name and address on the key come from the identity you choose.",
  "keys.generate.noIdentity":
    "You have no address a key can be made for. A key names one address, and a domain-wide identity has none.",
  "keys.generate.identity": "Address",
  "keys.generate.expiry": "Expires",
  "keys.generate.expiry.1y": "In 1 year",
  "keys.generate.expiry.2y": "In 2 years",
  "keys.generate.expiry.5y": "In 5 years",
  "keys.generate.expiry.never": "Never",
  "keys.generate.bind": "Sign my mail from this address with it",
  "keys.generate.submit": "Make key",
  "keys.generate.busy": "Making the key…",
  "keys.importOwn.open": "Import your own key…",
  "keys.importOwn.title": "Import your own key",
  "keys.importOwn.explain":
    "Paste a private key exported from another mail app, or choose the file. If it is protected, enter its passphrase: it is used once and not kept.",
  "keys.importOwn.keyLabel": "Private key",
  "keys.importOwn.fileLabel": "Or choose a key file (.asc, .pgp or .gpg)",
  "keys.importOwn.passphrase": "Passphrase (if the key has one)",
  "keys.importOwn.empty": "Paste a key or choose a file first.",
  "keys.importOwn.submit": "Import",
  "keys.importOwn.busy": "Importing the key…",
  "keys.theirs.heading": "Correspondents' keys",
  "keys.theirs.explain":
    "Keys this server holds for people you write to. Encrypted mail to them uses these, and their signatures are checked against them.",
  "keys.theirs.none": "You don't hold a key for anyone yet.",
  "keys.theirs.unavailable": "This server didn't list the keys it holds.",
  "keys.theirs.remove": "Remove…",
  "keys.theirs.removeFor": "Remove the key for {name}",
  "keys.theirs.removeTitle": "Remove the key for {name}?",
  "keys.theirs.removeWhat":
    "Mailroom stops using this key for {name} and keeps it in History.",
  "keys.theirs.removeRevives":
    "It comes back if they send it again, or publish it. To stop using it for good, choose “Don't use this key” instead.",
  "keys.theirs.removePinned":
    "It is the key encrypted mail to them uses, so you can't encrypt to them until there is another.",
  "keys.theirs.removeSubmit": "Remove key",
  "keys.theirs.removing": "Removing…",
  "keys.theirs.importAddress": "Import a key for someone: their address",
  "keys.theirs.import": "Import their key…",
  "keys.source.imported": "Imported by you",
  "keys.source.wkd": "From their key directory (WKD)",
  "keys.source.autocryptAuthenticated": "From their mail (Autocrypt, sender verified)",
  "keys.source.autocrypt": "From their mail (Autocrypt, sender not verified)",
  "keys.source.keyserver": "From a key server",
  "keys.source.other": "Source not recorded",
  "keys.done.generated": "Your new key is ready.",
  "keys.done.imported": "Your key was imported.",
  "keys.done.alreadyHeld": "You already had this key here.",
  "keys.done.bound": "The key is now used for {address}.",
  "keys.done.unbound": "The key is no longer used for any address.",
  "keys.done.published": "The key is published.",
  "keys.done.unpublished": "The key was withdrawn from the key directory.",
  "keys.done.extended": "The key now expires on {date}.",
  "keys.done.extendedNoDate": "The key was extended.",
  "keys.done.backedUp": "Downloaded {file}. Keep it, and its passphrase, somewhere safe.",
  "keys.done.retired": "The key was retired.",
  "keys.done.revoked": "The key was revoked.",
  "keys.done.removed": "The key for {name} was removed.",
  "keys.done.importedFor": "The key for {address} was imported.",
  "keys.error.keyManagementUnavailable":
    "This server can't do that: no key service is connected to it. Ask whoever runs it.",
  "keys.error.notFound": "That key isn't here any more. Reload the page and try again.",
  "keys.error.rateLimited": "Too many key operations in a row. Wait a minute and try again.",
  "keys.error.invalidArguments": "The server couldn't do that with what was entered. Check it and try again.",
  "keys.error.wildcardIdentity":
    "A key names one address, and a domain-wide identity has none. Choose a single address.",
  "keys.error.publicKeyRefused":
    "That is a public key. Your own key needs its private half; a correspondent's key goes under Correspondents' keys.",
  "keys.error.invalidKey":
    "That isn't a private key Mailroom can read, or the passphrase is wrong.",
  "keys.error.tooLarge": "That key is too big to import.",
  "keys.error.oneChangeAtATime": "Change one thing at a time.",
  "keys.error.keyIsDefault":
    "This key signs for an address. Choose another key for that address first, or retire this one, which also stops it signing.",
  "keys.error.keyIsPublished": "This key is published at its address. Withdraw it from the key directory first.",
  "keys.error.keyRetired": "A retired or revoked key can't sign, so it can't be used for an address.",
  "keys.error.wkdNotConfigured": "This server doesn't publish keys (its key directory, WKD, isn't set up).",
  "keys.error.notPublishable":
    "This key can't be published: it needs a single address at a domain this server publishes keys for.",
  "keys.error.retireIsPermanent": "A retired key can't be put back into use. Make or import another.",
  "keys.error.invalidProperties": "The server didn't accept that change.",
  "keys.error.unavailable": "Couldn't reach the server. Check your connection and try again.",
  "keys.error.failed": "That didn't work. Try again.",
  "keys.error.keyRevoked": "A revoked key can't be replaced. Make a new key for the address instead.",
  "keys.error.keyNotBound":
    "This key isn't the one signing for its address any more, so there's nothing to put a new key in place of. Make a new key instead.",
  "keys.error.wkdUnavailable": "This server can't look keys up in key directories (WKD).",
  "keys.error.keyDoesNotNameAddress":
    "This key doesn't name their address, so it can't be trusted for it.",

  // v1.93 final scope (#812 user IDs, #813 exact expiry, #815 history,
  // #817 replace, #818 refresh). Plain words; never a raw code.
  "keys.userIds.others": "Also on this key",
  "keys.userIds.revoked": "(revoked)",
  "keys.usedFor": "Used for {address}",
  "keys.usedForNone": "Not used for any address",
  "keys.notListed":
    "This key doesn't list {address}; some mail apps may refuse or flag it.",
  "keys.done.boundNotListed":
    "The key is now used for {address}. This key doesn't list {address}; some mail apps may refuse or flag it.",
  "keys.expiry.date": "On a date…",
  "keys.expiry.dateLabel": "Expiry date",
  "keys.expiry.dateUtc": "The key stops working at the end of that day (UTC).",
  "keys.expiry.datePreview": "Expires on {date}, in {days} days.",
  "keys.expiry.datePreviewOne": "Expires on {date}, tomorrow.",
  "keys.expiry.dateSoon":
    "That is inside the {days}-day warning window, so the key will show “Expires soon” straight away.",
  "keys.expiry.dateInvalid": "Choose a date.",
  "keys.expiry.dateTooSoon": "Choose a date from {min}: a key has to last at least a day.",
  "keys.expiry.dateTooFar": "Choose a date up to {max}: a key can last at most 10 years.",
  "keys.action.replace": "Replace…",
  "keys.replace.title": "Replace this key",
  "keys.replace.titleFor": "Replace the key for {address}",
  "keys.replace.what": "Mailroom makes a new key for {address} and uses it to sign your mail from now on.",
  "keys.replace.published": "The new key is published in the key directory in place of this one.",
  "keys.replace.notPublished": "This key isn't published, so the new one won't be either.",
  "keys.replace.old":
    "This key stops signing and is taken out of the key directory, but it still opens mail already encrypted to it.",
  "keys.replace.stolen": "If you think this key was stolen, revoke it instead.",
  "keys.replace.expiry": "The new key expires",
  "keys.replace.submit": "Replace key",
  "keys.replace.busy": "Making the new key…",
  "keys.replace.leftNewKey":
    "A new key was made but not put in place: it's listed below, unused. Your current key is unchanged.",
  "keys.done.replaced": "The key for {address} was replaced. The old one still opens past mail.",
  "keys.done.replacedPublished":
    "The key for {address} was replaced, and the new one is published. The old one still opens past mail.",
  "keys.history.heading": "History",
  "keys.history.yoursLabel": "your keys",
  "keys.history.theirsLabel": "correspondents' keys",
  "keys.history.yoursExplain":
    "Keys you no longer use. They still open mail that was encrypted to them.",
  "keys.history.yoursNone": "No retired, revoked or expired keys.",
  "keys.history.theirsExplain":
    "Keys you removed. A removed key comes back if they send or publish it again.",
  "keys.history.theirsNone": "No removed keys.",
  "keys.history.retiredOn": "Retired on {date}",
  "keys.history.retiredNotRecorded": "Retired (date not recorded)",
  "keys.history.replacedBy": "Replaced by",
  "keys.history.replacedByKey": "the key ending {tail}",
  "keys.history.removedOn": "Removed on {date}",
  "keys.history.show": "Show history",
  "keys.history.hide": "Hide history",
  "keys.history.showFor": "Show history of the key for {name}",
  "keys.history.hideFor": "Hide history of the key for {name}",
  "keys.history.loading": "Loading the history…",
  "keys.history.empty": "Nothing has been recorded for this key yet.",
  "keys.history.list": "History of the key for {name}",
  "keys.event.firstSeen": "First seen {via}",
  "keys.event.imported": "You imported it",
  "keys.event.renewed": "Renewed {via}",
  "keys.event.renewedUntil": "Renewed {via} — now expires {date}",
  "keys.event.updated": "Updated {via}",
  "keys.event.revokedSeen": "Revoked by its owner (seen {via})",
  "keys.event.revokedSeenPlain": "Revoked by its owner",
  "keys.event.pinned": "You chose to use it",
  "keys.event.refused": "You chose not to use it",
  "keys.event.removed": "You removed it",
  "keys.event.other": "Changed",
  "keys.via.autocrypt": "via Autocrypt",
  "keys.via.wkd": "via WKD",
  "keys.via.manual": "when you imported it",
  "keys.via.keyserver": "via a key server",
  "keys.via.user": "by you",
  "keys.via.other": "",
  "keys.refresh.check": "Check for an updated key",
  "keys.refresh.checkFor": "Check for an updated key for {name}",
  "keys.refresh.busy": "Checking for an updated key…",
  "keys.refresh.renewed": "Updated — now expires {date}",
  "keys.refresh.renewedNever": "Updated — it no longer expires",
  "keys.refresh.unchanged": "No newer version published",
  "keys.refresh.differentKey": "They've published a different key",
  "keys.refresh.revoked": "They've revoked this key. Don't encrypt to it any more.",
  "keys.refresh.updated": "Updated (a new address or subkey), with the same expiry",
  "keys.refresh.notFound": "Their key directory doesn't publish a key for them",
  "keys.refresh.unreachable": "Couldn't reach their key directory. Try again later.",
  "keys.refresh.unknown": "Checked, but this version of Mailroom can't describe the answer",
  "keys.refresh.rateLimited": "Checked recently — try again in a few minutes",
  "keys.refresh.decisionHeading": "{name} has published a different key.",
  "keys.refuse.action": "Don't use this key…",
  "keys.refuse.actionFor": "Don't use this key for {name}",
  "keys.refuse.title": "Don't use the key for {name}?",
  "keys.refuse.what":
    "Mailroom won't encrypt to this key or trust its signatures. It stays in your keys, and stays refused if it arrives again.",
  "keys.refuse.undo": "You can change your mind here later.",
  "keys.refuse.submit": "Don't use this key",
  "keys.refuse.busy": "Saving your choice…",
  "keys.trust.action": "Use this key again",
  "keys.trust.actionFor": "Use this key again for {name}",
  "keys.done.refused": "Mailroom won't use the key for {name}.",
  "keys.done.trusted": "Mailroom uses the key for {name} again.",

  // The desk's key-expiry banner (#795).
  "keyWarning.expiresOn": "Your key for {who} expires on {date}.",
  "keyWarning.expiresOnKey": "Your key {who} expires on {date}.",
  "keyWarning.expiresSoon": "Your key for {who} expires soon.",
  "keyWarning.expiresSoonKey": "Your key {who} expires soon.",
  "keyWarning.expiredOn":
    "Your key for {who} expired on {date}. People can no longer encrypt mail to you with it.",
  "keyWarning.expiredOnKey": "Your key {who} expired on {date}.",
  "keyWarning.expired": "Your key for {who} has expired. People can no longer encrypt mail to you with it.",
  "keyWarning.expiredKey": "Your key {who} has expired.",
  "keyWarning.open": "Extend it",
  "keyWarning.openExpired": "Extend or replace it",
  "keyWarning.dismiss": "Dismiss",
  "keyWarning.dismissFor": "Dismiss the warning about the key for {who}",

  // DATA. Export and deletion are mailroom-admin operations against a whole
  // login, not something a mail session is scoped to do — so the row names who
  // to ask rather than offering a button that would fail.
  "settings.data.operator":
    "Exporting or deleting everything in this login is run by your operator, not from here.",

  // ---- ACCOUNTS (#374 increment 3) ----------------------------------------
  //
  // Two screens: a list of accounts, and one account's controls. Every control
  // states its VALUE before its options, which is why several of these come in
  // pairs — the sentence that says where you are, then the thing that changes it.
  "settings.account.back": "← Accounts",
  "settings.account.none":
    "No accounts have arrived yet. If you have one, its identities are still syncing.",
  "settings.account.notFound":
    "That account is not in your list — it may have been removed in another tab.",
  "settings.account.noAddress": "No sending address yet.",
  "settings.account.lastSynced": "last synced {ago}",
  "settings.account.neverSynced": "never synced",
  "settings.account.working": "Working…",

  "settings.account.state": "State",
  "settings.account.state.ok": "Syncing. Last successful sync {ago}.",
  "settings.account.state.okNever": "Syncing is on, and nothing has come back on this account yet.",
  // What pausing COSTS, spelled out: the word on its own reads as harmless, and the
  // thing it stops is the user's mail arriving.
  "settings.account.state.paused":
    "Sync is paused. Nothing new arrives for this account until you resume it; everything already here stays, and stays readable.",
  "settings.account.state.failing": "Sync is failing: {error}",
  "settings.account.state.failures.one": "1 attempt in a row has failed.",
  "settings.account.state.failures.other": "{count} attempts in a row have failed.",
  // Not "syncing normally". Telemetry nobody has read is not good news.
  "settings.account.state.unknown": "This account's state has not been read yet.",
  "settings.account.pause": "Pause sync",
  "settings.account.resume": "Resume sync",
  "settings.account.pauseFailed": "Could not pause this account",
  "settings.account.resumeFailed": "Could not resume this account",
  // A syncing Gmail account is still pulling in its history (#914): how far
  // the newest-first sweep has got, and what the server's own pacing against
  // Google's daily download limit says about when it is done. A null field is
  // the server saying it cannot say yet, so the sentence drops the clause
  // rather than guessing.
  "settings.account.gmailImport.progress":
    "Importing your Gmail: {done} of {total} messages so far.",
  "settings.account.gmailImport.progressUnknownTotal":
    "Importing your Gmail: {done} messages so far.",
  "settings.account.gmailImport.eta":
    "About {days} more days, paced to stay within Google's daily download limit.",
  "settings.account.gmailImport.etaSoon": "Should finish within a day.",
  "settings.account.gmailImport.done": "Your Gmail history is fully imported.",

  // Ingest-only mode (ADR-028, #521). The status line reads the EFFECTIVE
  // flag from health, not the checkbox's own column — the two can disagree
  // once an instance-level switch is set (ADR-028 §2), and only health says
  // what actually happened to the last write.
  "settings.account.writeback": "Writeback",
  "settings.account.writeback.writing": "Mailroom syncs and writes to this account normally.",
  "settings.account.writeback.shadowed": "Ingest only: Mailroom is not writing to this account.",
  "settings.account.writeback.suppressed.one": "1 change recorded but not sent.",
  "settings.account.writeback.suppressed.other": "{count} changes recorded but not sent.",
  "settings.account.writeback.unknown":
    "This account's writeback state has not been read yet.",
  "settings.account.writeback.unread":
    "This control cannot be used until this account's record arrives.",
  "settings.account.writeback.label": "Ingest only (never write to this server)",
  "settings.account.writeback.explain":
    "Mailroom syncs this account but never modifies it. Flags mirror the server; labels, filing and trash stay local to Mailroom.",
  "settings.account.writeback.enableFailed": "Could not turn on ingest only for this account",
  "settings.account.writeback.disableFailed": "Could not turn off ingest only for this account",

  "settings.account.color": "Colour",
  "settings.account.color.none": "none · using the standard tone",
  "settings.account.color.unread": "The colour cannot be read until this account's record arrives.",
  "settings.account.color.pick": "Use {name}",
  "settings.account.color.blue": "blue",
  "settings.account.color.orange": "orange",
  "settings.account.color.teal": "teal",
  "settings.account.color.purple": "purple",
  "settings.account.color.custom": "Any colour",
  "settings.account.color.clear": "Clear",
  "settings.account.color.explain":
    "The dot beside this account's mail. The account's name is always shown next to it — the colour is a cue, never the only thing that tells two accounts apart.",
  "settings.account.colorFailed": "Could not change this account's colour",

  "settings.account.server": "Server",
  "settings.account.server.imap": "incoming {host} port {port}",
  "settings.account.server.smtp": "outgoing {host} port {port}",
  // The username's absence is the SERVER's design, said out loud: Account/get never
  // returns it, so leaving it off without a word would read as an empty field.
  "settings.account.server.noUsername":
    "The username is not shown: it is stored sealed, and the server never sends it back.",
  "settings.account.server.unread": "This account's server settings have not arrived yet.",
  "settings.account.credentials": "Change credentials…",
  "settings.account.credentialsSave": "Check and save",
  "settings.account.credentialsFailed": "Could not change these credentials",
  "settings.account.credentialsRefused":
    "The server would not accept these details, so nothing was changed.",
  "settings.account.password.hint":
    "Sent once and sealed on the server. It is never sent back, which is why it has to be typed again here.",

  "settings.account.folders": "Folders synced",
  "settings.account.folders.explain":
    "A folder with syncing paused is a frozen snapshot: it keeps everything it already had and gains nothing new, and nothing in it is removed either.",
  "settings.account.folders.none":
    "This login has no folders beyond the standard ones, which always sync.",
  "settings.account.folders.syncing": "syncing",
  "settings.account.folders.paused": "paused",

  "settings.account.identities": "Identities",
  "settings.account.identities.open": "Open Identities and signatures",

  "settings.account.provider": "Provider profile",
  "settings.account.provider.none": "none",
  "settings.account.provider.unread": "not read yet",
  "settings.account.provider.explain":
    "Chosen when the account was added and read-only here: it decides the provider quirks Mailroom works around while syncing.",

  "settings.account.rules": "Rules",
  // A FINDING rather than a placeholder: a rule's conditions have no account
  // selector, so "the rules for this account" is not a set that exists.
  "settings.account.rules.loginWide": "rules are login-wide",
  "settings.account.rules.count.one": "1 rule, and it applies to every account.",
  "settings.account.rules.count.other": "{count} rules, and they apply to every account.",
  "settings.account.rules.open": "Open Rules",

  "settings.account.notifications": "Notifications",
  // #59: the per-account switch is real; this line says where the rest is.
  "settings.account.notifications.where":
    "Which devices notify, what they notify about and quiet hours are in the Notifications row.",

  "settings.account.remove": "Remove this account",
  "settings.account.remove.open": "Remove account…",
  "settings.account.remove.title": "Remove {name}?",
  // ⚠ THE SERVER'S OWN LIST, from internal/store/accounts.go DeleteAccount — every
  // clause is one line of that comment, including the two that are easy to get
  // wrong: shared messages are KEPT, and a bound key survives with its link cleared.
  "settings.account.remove.body":
    "This removes {name}, its folders, its send-as identities and the signatures bound to them, and its outbound history. Messages that live only in this account are deleted permanently — not moved to Trash. A message that also exists in another account is kept there. An OpenPGP key bound to one of these identities is kept; only its link to the identity goes.",
  "settings.account.remove.confirm": "Remove the account",

  // ---- AREAS AND DRAWERS (#374 increment 3) -------------------------------
  "settings.areas.default": "Default drawer period",
  "settings.areas.default.value.one":
    "New areas, and every area still set to the default, auto-archive after 1 day.",
  "settings.areas.default.value.other":
    "New areas, and every area still set to the default, auto-archive after {count} days.",
  "settings.areas.default.reset": "Reset to {n} days",
  "settings.areas.default.explain":
    "It applies to every area whose own period is “default”, so changing it moves those areas too — not only new ones.",
  "settings.areas.default.unread": "The login default has not arrived from the server yet.",
  "settings.areas.default.failed": "Could not change the default drawer period",
  "settings.areas.days.one": "1 day",
  "settings.areas.days.other": "{count} days",
  "settings.areas.custom": "Custom…",
  "settings.areas.apply": "Apply",
  "settings.areas.never": "never",
  "settings.areas.defaultOption": "default ({n} days)",
  "settings.areas.defaultOptionUnread": "default",
  // The master switch, once at the top of the row. It makes every period below it
  // conditional, so it cannot be left to the per-card repeat alone.
  //
  // ⚠ NOT ONLY THE OPERATOR (#626 §A). `drawerSweepEnabled` is written through
  // the ordinary DeskPreferences write path (internal/api/desk_preferences.go),
  // not an admin-only one, and finishing Day-one setup with a drawer period
  // other than "never" turns it on that way (ui/setup/SetupDialsStep.tsx open()
  // sets `drawerSweepEnabled: true`). This row itself offers no direct toggle —
  // see this file's own note above the sweep's master-switch comment for why —
  // so the sentence names the two paths that actually exist rather than the one
  // that sounds simplest.
  // Learned placement's off switch (#714, settings/rows/LearnedPlacementToggle).
  // The toggle's words ARE its accessible name (WCAG 2.5.3), so they say what
  // happens, not the feature's internal name.
  "settings.learned.title": "Learned sorting",
  "settings.learned.toggle": "Sort new mail into areas by learning from your moves",
  "settings.learned.explain":
    "When you have moved a sender’s mail into the same area at least twice, Mailroom files that sender’s new mail there too. Moving one back teaches it otherwise. Rules you write always come first.",
  "settings.learned.failed": "Could not change learned sorting",

  // FOLLOW-UP REMINDERS (#58), on the in-tray settings (rows/FollowUpSettings.tsx).
  "settings.followUp.title": "Follow-up reminders",
  "settings.followUp.outbound": "Remind me when no one has answered my mail for a week",
  "settings.followUp.inbound": "Remind me about mail I haven't replied to",
  "settings.followUp.inboundDays": "Remind me after",
  "settings.followUp.days.one": "{count} day",
  "settings.followUp.days.other": "{count} days",
  "settings.followUp.explain":
    "Reminders appear on the desk under Follow up. Done hides one for good; Snooze brings it back later. A reminder stops by itself a month after the message.",
  "settings.followUp.inboundExplain":
    "Off unless you turn it on: on real mail, only about one in six of the messages it picks were ones that got a reply.",
  "settings.followUp.failed": "Could not change follow-up reminders",
  "settings.state.followUp.both": "follow-up reminders on, both kinds",
  "settings.state.followUp.outbound": "follow-up reminders on for unanswered mail",
  "settings.state.followUp.inbound": "follow-up reminders on for mail you haven't replied to",
  "settings.state.followUp.off": "follow-up reminders off",
  "settings.areas.sweepOffRow":
    "Sweeping is switched off for this login, so no drawer auto-archives anything whatever the periods below say. Choosing a drawer period in Day-one setup turns it on; your operator can too.",
  "settings.areas.noneYet":
    "No areas yet. Everything on the desk sits under “Needs sorting” until an area claims it.",
  "settings.areas.rename": "Rename",
  "settings.areas.moveUp": "Move {name} earlier",
  "settings.areas.moveDown": "Move {name} later",
  "settings.areas.fedBy": "Fed by",
  "settings.areas.ownLabel": "its own label {name} — always, and it cannot be removed",
  "settings.areas.ownLabelUnread": "its own label, which has not loaded yet",
  "settings.areas.noOtherLabels": "You have no other labels to point at this area yet.",
  // A hint from the client's own copy of the areas, not a prediction: the same
  // fact the server's refusal would state, shown before the click rather than
  // after it. The box stays clickable — see the comment at it.
  "settings.areas.feedsOther": "feeds {name}",
  "settings.areas.feedExclusive":
    "A tab or label feeds one area at a time. Choosing one that another area already has is refused, and the refusal names that area.",
  "settings.areas.drawer": "Auto-archives after",
  "settings.areas.resetToDefault": "Use the login default",
  "settings.areas.drawerPromise":
    "A drawer archives; it never deletes anything and never marks anything read, and archived mail stays in All mail and in search.",
  "settings.areas.drawerPromiseCounted": "{counted} · never deletes · never marks read",
  "settings.areas.archivedSoFar.one": "archived 1 message so far",
  "settings.areas.archivedSoFar.other": "archived {count} messages so far",
  "settings.areas.hideFromRail": "Hide from the rail",
  "settings.areas.hideFromRail.explain":
    "It keeps its sections on the desk and its mail still lands here; only the navigation entry goes.",
  "settings.areas.rulesHere": "Rules that place mail here",
  "settings.areas.rulesUnread": "The rules have not been read yet.",
  "settings.areas.rulesNone":
    "No rule applies this area's own label. Mail arrives here through the feeds above.",
  "settings.areas.ruleDisabled": "switched off",
  "settings.areas.delete": "Delete area…",
  "settings.areas.delete.title": "Delete {name}?",
  // The server's semantics: the own label is immutable and survives the destroy.
  "settings.areas.delete.body":
    "The area's label stays on every message that carries it; only the area goes. Its mail is then claimed by no area, so it appears under Needs sorting until one claims it.",
  "settings.areas.delete.confirm": "Delete the area",
  "settings.areas.new": "Add area",
  "settings.areas.newPlaceholder": "Name for the new area",
  // ⚠ NOT VISIBLE ELSEWHERE. A new area's own label is minted MirrorToIMAP:
  // false (internal/store/areas.go CreateArea/AreaRow), so it never gets a
  // MailroomLabel-<slug> IMAP keyword and no other mail client can see it —
  // deliberately, so an area's own label cannot collide with a folder the
  // provider already has. Only a label the user points AT an area some other
  // way (settings.areas.assignTo) can carry one.
  "settings.areas.new.explain":
    "Mailroom makes a label of the same name and gives the area that label. Putting mail in the area is applying it — a Mailroom-only label, not one any other mail client can see.",
  "settings.areas.homeless": "Labels in no area",
  "settings.areas.homeless.explain":
    "These labels feed no area, so mail carrying them is placed by its category or not at all.",
  "settings.areas.assignTo": "Assign {name} to an area",
  "settings.areas.assignTo.prompt": "Assign to…",
  "settings.areas.needsSorting": "Needs sorting",
  "settings.areas.sortNow": "Open the sort queue",
  // Day-one setup, reachable from the desk's own row (#446): the door "import
  // later" promised, and the way to run the whole wizard again.
  "settings.areas.dayOne": "Day-one setup",
  "settings.areas.dayOne.explain":
    "Import a folder you left alone, or go through the four steps again. Nothing is written until you choose it.",
  "settings.areas.import": "Import a folder",
  "settings.areas.import.pick": "Which folder to import",
  "settings.areas.import.prompt": "Choose a folder…",
  "settings.areas.import.inAccount": "{folder} ({account})",
  "settings.areas.import.none": "No folders to import.",
  "settings.areas.runSetup": "Run setup again",

  // ---- THE LINE (#374 increment 3) ----------------------------------------
  "settings.line.threshold": "Where the line is",
  "settings.line.unread":
    "The line has not been read from the server yet, so it cannot be moved from here.",
  "settings.line.reset": "Reset to {value}",
  "settings.line.explain":
    "Mail scored at or above the line is on the surface, waiting for a decision; below it, it is kept in that area's drawer. Nothing moves until you apply a change.",
  "settings.line.facts": "What this line does",
  "settings.line.week": "last 7 days: {review} to review · {drawers} to drawers",
  "settings.line.week.asking": "Counting the last 7 days…",
  "settings.line.week.failed": "The last 7 days could not be counted on this server.",
  "settings.line.stricter.one":
    "one step stricter ({threshold}) would move 1 more message to drawers",
  "settings.line.stricter.other":
    "one step stricter ({threshold}) would move {count} more messages to drawers",
  // A MEASURED zero, and it gets words rather than a 0: both sides were counted and
  // they agree, which is a different statement from "nobody asked".
  "settings.line.stricterNothing": "one step stricter ({threshold}) would move nothing",

  // ---- IN-TRAY (#374 increment 3) -----------------------------------------
  "settings.tray.nag": "Nag after",
  "settings.tray.unread": "The tray's nag setting has not arrived from the server yet.",
  "settings.tray.reset": "Reset to {n} days",
  "settings.tray.explain":
    "How long something may sit in the tray before the strip above your desk starts saying so. It never moves or deletes anything.",
  "settings.tray.failed": "Could not change the tray nag",
  // The long form of the state line's clause. True today, and the reason it is said
  // at all: a holding area suspected of losing things is worse than one that admits
  // what it does not collect.
  "settings.state.trayNotRouted.long":
    "Returned snoozes and bounces are not yet routed here; the tray is the flag. A snooze that wakes comes back to the list it left, and a bounce appears on the account's health.",

  "settings.theme": "Theme",
  "settings.theme.system": "Follow system",
  "settings.theme.light": "Light",
  "settings.theme.dark": "Dark",
  "settings.density": "Density",
  "settings.density.comfortable": "Comfortable",
  "settings.density.cozy": "Cozy",
  "settings.density.compact": "Compact",
  // TEXT SIZE (#514). The five words are BOTH the buttons and the slider
  // `aria-valuetext`, on purpose: what a screen reader is told and what a
  // sighted user reads are the same five words, so a support conversation about
  // "Large" is about one thing. "Default" rather than "Medium" because the
  // middle step is not a size anyone chose — it is the size the app has always
  // drawn at, and naming it that is what makes going back obvious.
  "settings.textSize": "Text size",
  "settings.textSize.xs": "Smallest",
  "settings.textSize.s": "Small",
  "settings.textSize.m": "Default",
  "settings.textSize.l": "Large",
  "settings.textSize.xl": "Largest",
  // ROWS (#514).
  "settings.rows": "Rows",
  "settings.rows.oneLine": "One line",
  "settings.rows.multiLine": "Multi-line",
  "settings.rows.hint":
    "One line puts the sender, the subject and then as much of the preview as the subject leaves room for. Multi-line gives the subject and the preview a line each.",
  "settings.shortcuts": "Keyboard shortcuts",

  "compose.markdownMode": "Markdown",

  // Unsubscribe (#58). The explain line is the point of the confirm step: an
  // unsubscribe is a reply, and a reply tells a sender the address is real.
  "unsubscribe.open": "Unsubscribe",
  "unsubscribe.explain":
    "Unsubscribing tells this sender your address is real and being read. For a sender who had your permission that is fine; for one who did not, it is worth more to them than the unsubscribe.",
  "unsubscribe.openLink": "Open {host}",
  "unsubscribe.sendMail": "Email {to}",
  "unsubscribe.sending": "Sending…",
  "unsubscribe.sent": "Unsubscribe request sent.",
  "unsubscribe.queued": "You're offline — the unsubscribe request will be sent when you reconnect.",
  "unsubscribe.defaultBody": "Please unsubscribe this address.",

  // Calendar invitations (#725). The card above a message that carries an
  // invitation, a cancellation or an attendee's reply. "Maybe" is the button;
  // the iTIP word is TENTATIVE, which nobody says.
  "invite.region": "Calendar invitation",
  "invite.cancelledRegion": "Cancelled event",
  "invite.replyRegion": "Reply to an invitation",
  "invite.noTitle": "(no title)",
  "invite.when": "When",
  "invite.where": "Where",
  "invite.organizer": "Organiser",
  "invite.guests": "Guests",
  "invite.guestsOmitted": "and {count} more",
  "invite.repeatsLabel": "Repeats",
  "invite.allDay": "{date} (all day)",
  "invite.floatingZone": "{time} ({zone} — shown as written)",
  "invite.floatingNoZone": "{time} (no time zone given — shown as written)",
  "invite.state.cancelled": "This event has been cancelled.",
  "invite.state.newer": "There is a newer version of this invitation — the details here may be out of date.",
  "invite.state.updates": "This updates an earlier invitation.",
  "invite.state.updated": "This is an updated invitation.",
  "invite.unverified.update":
    "This update could not be verified as coming from the organiser, so it changes no other copy of the invitation.",
  "invite.unverified.reply": "This reply could not be verified as coming from the guest it names.",
  "invite.respond": "Respond to this invitation",
  "invite.accept": "Accept",
  "invite.tentative": "Maybe",
  "invite.decline": "Decline",
  "invite.sending": "Sending your answer…",
  "invite.explainSend": "Your answer is sent to {organizer} from {from}.",
  "invite.you.ACCEPTED": "You accepted.",
  "invite.you.TENTATIVE": "You said maybe.",
  "invite.you.DECLINED": "You declined.",
  "invite.you.earlier": "{answer} That was for an earlier version.",
  "invite.replyNotSent": "Your answer has not been sent — see the Outbox.",
  "invite.sent.ACCEPTED": "Accepted — your answer is on its way to {organizer}.",
  "invite.sent.TENTATIVE": "Maybe — your answer is on its way to {organizer}.",
  "invite.sent.DECLINED": "Declined — your answer is on its way to {organizer}.",
  "invite.cannot.notAddressedToYou":
    "This invitation was not sent to an address you can send from, so it cannot be answered here.",
  "invite.cannot.noOrganizer": "This invitation names no valid organiser, so there is nobody to answer.",
  "invite.cannot.noUid": "This invitation has no event identifier, so it cannot be answered.",
  "invite.problem.tooLarge": "This message carries a calendar invitation too large to read safely.",
  "invite.problem.unreadable": "This message carries a calendar invitation that could not be read.",
  "invite.partstat.ACCEPTED": "accepted",
  "invite.partstat.TENTATIVE": "maybe",
  "invite.partstat.DECLINED": "declined",
  "invite.partstat.NEEDS-ACTION": "no answer yet",
  "invite.partstat.DELEGATED": "delegated",
  "invite.replied.ACCEPTED": "{who} accepted.",
  "invite.replied.TENTATIVE": "{who} said maybe.",
  "invite.replied.DECLINED": "{who} declined.",
  "invite.replied.other": "{who} replied.",
  "invite.repeats.generic": "Repeats",
  "invite.repeats.DAILY": "Repeats daily",
  "invite.repeats.WEEKLY": "Repeats weekly",
  "invite.repeats.MONTHLY": "Repeats monthly",
  "invite.repeats.YEARLY": "Repeats yearly",
  "invite.repeats.every": "Repeats every {interval} {unit}",
  "invite.unit.DAILY": "days",
  "invite.unit.WEEKLY": "weeks",
  "invite.unit.MONTHLY": "months",
  "invite.unit.YEARLY": "years",
  "invite.repeats.onDays": "{base} on {days}",
  "invite.repeats.count": "{base}, {count} times",
  "invite.repeats.until": "{base}, until {date}",
  "invite.ordinal.1": "first",
  "invite.ordinal.2": "second",
  "invite.ordinal.3": "third",
  "invite.ordinal.4": "fourth",
  "invite.ordinal.last": "last",

  // Send later (#56). Every row shows the instant it resolves to — deferring your
  // own mail to the wrong hour means a message that goes out at 3am.
  "sendLater.title": "Send later",
  "sendLater.open": "Send later…",
  "sendLater.noneAvailable": "No scheduling options are available right now.",
  "sendLater.scheduled": "Scheduled to send {when}.",

  // Signatures and templates (#56). A signature is bound to an address, so the
  // picker says which one it belongs to; a template refuses to overwrite something
  // you already typed, and says so rather than silently doing nothing.
  "snippets.title": "Signatures and templates",
  "snippets.explain":
    "A signature is added below what you write; the one you mark as default is added for you. A template is a whole message you start from. Bind a signature to one of your addresses and it is only used when you send from that address.",
  "snippets.insertSignature": "Signature",
  "snippets.insertTemplate": "Template",
  "snippets.pickSignature": "Insert a signature",
  "snippets.pickTemplate": "Start from a template",
  "snippets.templateNeedsEmptyBody":
    "A template is a whole message, so it is only offered before you start writing.",
  "snippets.noSignatures": "You have not saved a signature for this address yet.",
  "snippets.noTemplates": "You have not saved a template yet.",
  "snippets.forThisAddress": "this address",
  "snippets.default": "default",
  "snippets.signatures": "Signatures",
  "snippets.templates": "Templates",
  "snippets.none": "Nothing saved yet.",
  "snippets.add": "New",
  "snippets.name": "Name",
  "snippets.body": "Text",
  "snippets.identity": "Use with",
  "snippets.anyIdentity": "Any address",
  "snippets.isDefault": "Use this one automatically",
  "snippets.deleteConfirm": "Delete “{name}”?",
  "snippets.nameRequired": "Give it a name so you can find it again.",
  "snippets.bodyRequired": "It needs some text.",

  // #202. The list is the other half of remembering a decision: an "always allow"
  // you cannot see or take back is the permanent consent the blocking default
  // exists to prevent.
  "remoteAllows.title": "Senders whose images load",
  "remoteAllows.explain":
    "Remote images stay blocked for everyone else. These are the senders you chose to always load, and you can stop any of them here.",
  "remoteAllows.empty":
    "Nothing remembered � every message still asks before loading remote images.",
  "remoteAllows.anyoneAt": "Anyone at {domain}",
  "remoteAllows.broad": "whole domain",
  "remoteAllows.revoke": "Stop allowing",
  "settings.paneLayout": "Reading pane",
  "settings.paneLayout.vertical": "Beside the list",
  "settings.paneLayout.horizontal": "Below the list",
  "settings.paneLayout.off": "No reading pane",
  "settings.paneResize": "Resize the reading pane",
  // Web Push notifications (Forgejo #42). "This device" is deliberate: a push
  // subscription is per browser/device, not per account.
  "settings.notifications": "Notifications",
  "settings.notifications.enable": "Enable notifications",
  "settings.notifications.disable": "Turn off notifications",
  "settings.notifications.on": "New-mail notifications are on for this device.",
  "settings.notifications.off": "Get notified about new mail on this device.",
  "settings.notifications.blocked":
    "Notifications are blocked for this site. Allow them in your browser settings, then try again.",
  "settings.notifications.unsupported": "This browser can’t show push notifications.",
  "settings.notifications.unavailable": "This server doesn’t offer push notifications.",
  "settings.notifications.working": "Working…",
  "settings.notifications.enabled": "Notifications are on for this device.",
  "settings.notifications.disabled": "Notifications turned off for this device.",
  "settings.notifications.error": "Could not change notifications: {error}",
  // ---- #59: device mode, quiet hours, per account ---------------------------
  "settings.notify.mode.legend": "This device notifies you about",
  "settings.notify.mode.all": "Every new message",
  "settings.notify.mode.above_line": "Mail above the line",
  "settings.notify.mode.vip": "Mail from VIPs",
  "settings.notify.mode.above_line_or_vip": "Mail above the line or from VIPs",
  "settings.notify.mode.hint":
    "Above the line means the desk’s own line, plus mail a “Mark important” rule marked. Mail the desk has not judged yet does not count.",
  "settings.notify.mode.refused": "the server did not change this device",
  "settings.quiet.legend": "Quiet hours",
  "settings.quiet.enabled": "Turn on quiet hours",
  "settings.quiet.from": "From",
  "settings.quiet.until": "Until",
  "settings.quiet.days": "Starting on",
  "settings.quiet.day.mon": "Monday",
  "settings.quiet.day.tue": "Tuesday",
  "settings.quiet.day.wed": "Wednesday",
  "settings.quiet.day.thu": "Thursday",
  "settings.quiet.day.fri": "Friday",
  "settings.quiet.day.sat": "Saturday",
  "settings.quiet.day.sun": "Sunday",
  "settings.quiet.zone": "Time zone",
  "settings.quiet.allowVip": "Allow VIPs during quiet hours",
  "settings.quiet.hint":
    "During quiet hours every device, whatever its setting above, notifies you only about mail from a VIP. If “Allow VIPs during quiet hours” is off, no device notifies you at all. A period that starts late runs into the next morning. Times follow your time zone’s clock, daylight saving included.",
  "settings.quiet.save": "Save quiet hours",
  "settings.quiet.failed": "Could not save quiet hours",
  "settings.quiet.sameTimes": "The start and end times are the same, so there would be no quiet period.",
  "settings.quiet.noDays": "Choose at least one day for quiet hours to start on.",
  "settings.notify.account.legend": "Accounts that notify",
  "settings.notify.account.toggle": "Notify me about new mail in {name}",
  "settings.notify.account.hint": "An account switched off here notifies no device.",
  "settings.notify.account.failed": "Could not change notifications for that account",

  "settings.senders": "Senders",
  "settings.senders.vips": "VIPs",
  "settings.senders.blocked": "Blocked senders",
  "settings.senders.noVips": "No VIPs yet. Mark one here, or from a sender’s card.",
  "settings.senders.noBlocked": "Nobody is blocked.",
  "settings.senders.unavailable": "This server cannot list senders.",
  "settings.senders.toJunk": "new mail goes to Junk",
  "settings.senders.toTrash": "new mail goes to Trash",
  "settings.senders.removeVip": "Remove {address} from VIPs",
  "settings.senders.vipRemoved": "{address} is no longer a VIP.",
  "settings.senders.addLabel": "Mark an address as VIP",
  "settings.senders.add": "Mark as VIP",
  "settings.senders.unknown": "There is no mail from {address} yet, so there is no sender to mark.",
  "settings.senders.addFailed": "Could not look that address up.",
  "settings.senders.capped": "Showing {shown} of {total}.",
  "settings.senders.hint":
    "VIP mail can notify you during quiet hours and is what is:vip finds. A blocked sender’s new mail goes to Junk or Trash; it is matched by exact address.",
  "settings.senders.vipCount.one": "1 VIP",
  "settings.senders.vipCount.other": "{count} VIPs",
  "settings.senders.blockedCount": "{count} blocked",

  // ---- Out of office (#50) --------------------------------------------------
  "settings.outOfOffice": "Out of office",
  "oof.loading": "Reading each account’s out-of-office reply…",
  "oof.unavailable": "This server cannot set an out-of-office reply.",
  "oof.noAccounts": "There is no mail account to reply from.",
  "oof.hint.provider":
    "Your mail provider sends these replies, not Mailroom, so they work while Mailroom is closed. They do not count toward Mailroom’s sending quota meter, but your provider may count them toward its own daily limit.",
  "oof.enable": "Send an automatic reply",
  "oof.firstDay": "First day away",
  "oof.lastDay": "Last day away",
  "oof.hint.dates":
    "Optional. Days are whole days in your time zone. With no first day the reply starts when you save; with no last day it runs until you turn it off.",
  "oof.subject": "Subject",
  "oof.body": "Message",
  "oof.days": "Reply to the same sender at most once every this many days",
  "oof.hint.days": "From 1 to 30 days.",
  "oof.from": "Reply from",
  "oof.hint.addresses":
    "A reply goes only to mail sent to one of this account’s own addresses ({addresses}). Mail that reached a per-site catch-all address gets no reply. Mail your provider marks as spam gets no reply either.",
  "oof.contactsOnly": "Only reply to people I have written to or marked as VIP",
  "oof.hint.contacts":
    "Up to {max} people, most recently in touch first, and fewer if your provider’s size limit is smaller. Blocked senders never get a reply.",
  "oof.contacts.all": "The reply covers all {total} of them.",
  "oof.contacts.capped": "The reply covers {included} of {total}; the rest get no reply.",
  "oof.save": "Save the reply for {name}",
  "oof.saving": "Saving…",
  "oof.saved.on": "Saved. Your provider is now sending the reply.",
  "oof.saved.off": "Saved. The reply is off.",
  "oof.unsupported": "Not available for this account: {reason}.",
  "oof.state.off": "The reply is off.",
  "oof.state.on": "On: your provider is replying until you turn it off.",
  "oof.state.onUntil": "On until the end of {until}.",
  "oof.state.scheduled": "Starts on {from} and runs until you turn it off.",
  "oof.state.scheduledUntil": "Starts on {from} and ends after {until}.",
  "oof.state.active": "On.",
  "oof.state.displaced": "Another filter script is now active on your mail server, so the reply is not being sent.",
  "oof.state.missing": "The reply is no longer active on your mail server.",
  "oof.state.modified": "The reply on your mail server was changed outside Mailroom.",
  "oof.state.error": "Mailroom could not reach your mail server to check the reply.",
  "oof.err.firstDay": "The first day is not a date.",
  "oof.err.lastDay": "The last day is not a date.",
  "oof.err.order": "The last day is before the first day.",
  "oof.err.past": "The last day has already passed.",
  "oof.err.body": "Write the message before turning the reply on.",
  "oof.err.days": "Choose a whole number of days from 1 to 30.",
  "oof.err.failed": "Could not save the reply.",
  "oof.fact.off": "off",
  "oof.fact.on": "on for {name}",
  "oof.fact.drift": "changed on the server",

  // ---- Clean up (#53) -------------------------------------------------------
  "settings.cleanup": "Clean up",
  "settings.state.cleanup": "senders by volume and by how much you ignore",
  "cleanup.controls": "Which senders to show",
  "cleanup.groupBy": "Group by",
  "cleanup.groupBy.address": "Sender",
  "cleanup.groupBy.domain": "Domain",
  "cleanup.groupBy.list": "Mailing list",
  "cleanup.sort": "Rank by",
  "cleanup.sort.volume": "Most mail",
  "cleanup.sort.neglect": "Most ignored",
  "cleanup.window": "Received in",
  "cleanup.window.days": "the last {days} days",
  "cleanup.window.all": "all time",
  "cleanup.loading": "Counting…",
  "cleanup.failed": "Could not count your senders.",
  "cleanup.none": "No mail in this period.",
  "cleanup.showing": "Showing {shown} of {total}.",
  "cleanup.count.address.one": "{count} sender",
  "cleanup.count.address.other": "{count} senders",
  "cleanup.count.domain.one": "{count} domain",
  "cleanup.count.domain.other": "{count} domains",
  "cleanup.count.list.one": "{count} mailing list",
  "cleanup.count.list.other": "{count} mailing lists",
  "cleanup.ignoredMeans":
    "Ignored means still unread, or archived from Mailroom before you read it. Mail in Trash or Junk is not counted.",
  "cleanup.facts": "{total} messages · {unread} unread · {ignored} ignored · last {last}",
  "cleanup.addresses": "· from {count} addresses",
  "cleanup.isBlocked": "· blocked",
  "cleanup.more": "Show more",
  "cleanup.unsubscribe": "Unsubscribe…",
  "cleanup.archiveAll": "Archive all",
  "cleanup.moveAll": "Move all to…",
  "cleanup.block": "Block…",
  "cleanup.unblock": "Unblock",
  "cleanup.unblocked": "Unblocked {key}.",
  "cleanup.cannotAct": "This one cannot be searched for exactly, so there is nothing to act on.",
  "cleanup.blocked.junk": "Blocked {key} (addresses blocked: {count}). New mail from them goes to Junk.",
  "cleanup.blocked.trash": "Blocked {key} (addresses blocked: {count}). New mail from them goes to Trash.",
  "cleanup.block.more": "Older addresses not blocked: {count}.",
  "cleanup.everyoneAt": "· everyone at {domain}",
  "cleanup.block.pick": "Block these addresses",
  "cleanup.block.vip": "(VIP)",
  "cleanup.block.correspondent": "(you have written to them)",
  "cleanup.block.already": "(already blocked)",
  "cleanup.block.defaults":
    "VIPs and people you have written to are left unticked; tick them yourself to block them too.",
  "cleanup.block.noMoveForGroups":
    "Mail already here is not moved from here, because the search would take every address in this row. Use the address's own row to move it.",
  "cleanup.block.goOne": "Block 1 address",
  "cleanup.block.goMany": "Block {count} addresses",
  "cleanup.block.exact":
    "Blocking matches exact addresses: an address this sender starts using later is not blocked.",

  "shortcuts.title": "Keyboard shortcuts",
  "shortcuts.navigate": "Next / previous message",
  // #447. The list is a grid now, so it owns the arrows, the ends and the page
  // keys while focus is in it — and the halves are switched on the tabs
  // themselves rather than by Tab, which used to be swallowed inside the list and
  // therefore made every heading, drawer and line control unreachable.
  "shortcuts.listEnds": "First / last message in the list",
  "shortcuts.listPage": "A screenful of messages at a time",
  "shortcuts.open": "Open the selected message",
  "shortcuts.back": "Back to the list",
  "shortcuts.archive": "Archive",
  "shortcuts.trash": "Delete",
  // Toggles the same flag everywhere; the word differs by surface (`flagWord` in
  // ui/useListVerbs.ts) — "star" in the message list, "in-tray" on the desk and
  // the tray page, which is why both are named here.
  "shortcuts.star": "Toggle star (the in-tray flag, on the desk and in-tray lists)",
  "shortcuts.sendCompose": "Send the message you are composing",
  // #648, only meaningful on a reply/reply-all/forward (offersSendAndArchive) —
  // listed unconditionally like every other context-dependent key on this sheet.
  "shortcuts.sendArchiveCompose": "Send & archive the conversation you are replying to or forwarding",
  "shortcuts.pin": "Toggle pin",
  "shortcuts.snooze": "Snooze (in the Snoozed view: bring it back)",
  "shortcuts.important": "Correct the priority verdict (important / not important)",
  "shortcuts.mute": "Mute / unmute the conversation",
  // NOT A TOGGLE (#624 §1): Shift+I and Shift+U each set an absolute state —
  // doRead(true) / doRead(false) in ui/useListVerbs.ts — so pressing the same one
  // twice does nothing the second time, unlike every other key on this sheet
  // that says "toggle".
  "shortcuts.read": "Mark read / mark unread",
  // Space is the desk's and the in-tray's own key (ui/desk/gridKeys.ts): the
  // message list has no general Space shortcut, only the native behaviour of a
  // focused checkbox.
  "shortcuts.select": "Toggle selection (Space: the desk and in-tray lists only)",
  "shortcuts.selectRange": "Select a range",
  "shortcuts.selectOne": "Add or remove one message from the selection (the desk and in-tray lists)",
  "shortcuts.reply": "Reply",
  "shortcuts.replyAll": "Reply all",
  "shortcuts.forward": "Forward",
  "shortcuts.compose": "Compose",
  "shortcuts.search": "Search",
  "shortcuts.palette": "Command palette",
  // Every letter ui/goto.ts's GOTO_VIEWS binds — nine, the same set and order as
  // the kbd label below ("g then i/p/n/s/b/e/d/t/a"), not GOTO_VIEWS's own object
  // order. A review found "needs a category" and "archive" missing here (#624
  // §1), which made this the one place `g n` and `g a` were undiscoverable.
  "shortcuts.goto":
    "Go to inbox / priority / needs a category / starred / snoozed / sent / drafts / trash / archive",
  "shortcuts.refile": "Re-file into the Nth area (in Needs sorting: place it there)",
  "shortcuts.undoToast": "Move to the Undo button (the countdown pauses there)",
  "shortcuts.deskTabs":
    "Switch between Review first and Everything else (with a tab focused)",
  "shortcuts.switcher": "Open the account switcher",
  "shortcuts.scope": "Show all accounts / only the Nth account",
  "shortcuts.help": "This help",

  "undo.sending": "Sending…",
  "undo.unsend": "Undo send",

  // The sign-in landing page. It is NOT part of this bundle — it is rendered by
  // the Go server (internal/auth/page.go), because it has to be servable to an
  // UNAUTHENTICATED visitor and every byte of this bundle sits behind the login
  // gate. Its strings live here anyway, and internal/auth's page_test.go FAILS if
  // the two catalogs' signin.* keys ever drift apart: a translator gets one
  // catalog with every user-facing string in the product, front door included.
  "signin.title": "Sign in — Mailroom",
  "signin.tagline": "Your mail, on hardware you control.",
  "signin.heading": "Sign in",
  "signin.button": "Sign in",
  "signin.subheading":
    "Mailroom uses your identity provider. You will be sent there to sign in, then brought straight back.",

  "signin.error.denied":
    "That account cannot use this Mailroom: it has been disabled here, or your identity provider did not send an email address for it. Ask whoever runs this instance for help.",
  "signin.error.expired": "That sign-in took too long and has expired. Please start again.",
  "signin.error.failed": "Sign-in could not be completed, so no session was created. Please try again.",
  "signin.error.unprovisionable":
    "Your sign-in was verified, but Mailroom could not set up your account: its email address already belongs to a different account on this server. Signing in again will not fix it; it has to be fixed on the server, and whoever runs this Mailroom will find the details in its log.",
  "signin.notice.signedOut": "You are signed out.",

  "signin.devauth.title": "This instance has no authentication",
  "signin.devauth.body":
    "Dev-auth is enabled: every request that reaches this server is treated as the operator, with no sign-in and no session. It must not be reachable from an untrusted network. Configure the OIDC block to turn it off.",
  "signin.devauth.open": "Open Mailroom",

  "signin.theme": "Theme",
  "signin.theme.system": "System",
  "signin.theme.light": "Light",
  "signin.theme.dark": "Dark",

  // ---- T0 rules (#155) ----
  "rules.title": "Rules",
  "rules.none": "No rules yet. Rules file, label and flag mail as it arrives — the fastest way to write one is from a message you already have.",
  "rules.loading": "Loading your rules…",
  "rules.loadFailed": "Could not load your rules.",
  "rules.new": "New rule",
  "rules.evaluationOrder": "Rules run in this order. A rule that stops evaluation prevents every rule below it from running.",
  "rules.enabled": "Enabled",
  "rules.disabled": "Disabled",
  "rules.edit": "Edit",
  "rules.delete": "Delete",
  "rules.deleteConfirm": "Delete the rule “{name}”? Mail it has already labelled keeps its labels.",
  "rules.name": "Name",
  "rules.namePlaceholder": "What this rule is for",
  "rules.priority": "Order",
  "rules.priorityHint": "Lower runs first.",
  "rules.conditions": "When a message matches",
  "rules.actions": "Do this",
  "rules.addCondition": "Add a condition",
  "rules.addGroup": "Add a group",
  "rules.addAction": "Add an action",
  "rules.remove": "Remove",
  "rules.matchAll": "all of",
  "rules.matchAny": "any of",
  "rules.headerName": "Header",
  "rules.headerPlaceholder": "X-Some-Header",
  "rules.value": "Value",
  "rules.save": "Save rule",
  "rules.cancel": "Cancel",
  "rules.saved": "Rule saved.",
  "rules.created": "Rule created.",
  "rules.deleted": "Rule deleted.",
  "rules.needsName": "Give the rule a name.",
  "rules.needsAction": "A rule needs at least one action.",
  "rules.needsCondition": "Add at least one condition, or the rule would match nothing.",
  "rules.needsLabel": "Choose a label to apply, or the rule would run on every message and do nothing.",
  "rules.newLabel": "New label…",
  "rules.newLabelPlaceholder": "Label name",
  "rules.creating": "Creating…",
  "rules.labelCreateFailed": "The label was created but cannot be used in a rule yet.",
  "rules.inert": "This rule applies a label that no longer exists, so it does nothing.",

  "rules.field.from": "Sender",
  "rules.field.fromDomain": "Sender domain",
  "rules.field.to": "Recipient",
  "rules.field.subject": "Subject",
  "rules.field.alias": "Sent to alias",
  "rules.field.listId": "Mailing list (List-Id)",
  "rules.field.listUnsubscribe": "Has unsubscribe link",
  "rules.field.header": "Header…",

  "rules.op.equals": "is",
  "rules.op.contains": "contains",
  "rules.op.matches": "matches regex",
  "rules.op.exists": "exists",

  "rules.action.addLabel": "Apply label",
  "rules.action.markPriority": "Mark important",
  "rules.action.flag": "Star it",
  "rules.action.skipInbox": "Skip the inbox",
  "rules.action.archive": "Archive it",
  "rules.action.stop": "Stop processing further rules",

  "rules.preview.title": "Blast radius",
  "rules.preview.check": "Check what this matches",
  "rules.preview.checking": "Checking…",
  "rules.preview.exact": "Matches {matched} of your {total} messages.",
  "rules.preview.bounded": "Matches {matched} of the {scanned} most recent messages (you have {total}). Older mail was not checked.",
  "rules.preview.bodies": "This rule reads message bodies, so it checks a smaller window.",
  "rules.preview.none": "Matches nothing you have received.",
  "rules.preview.samples": "For example:",
  "rules.preview.stale": "Conditions changed — check again.",
  "rules.preview.conflicts": "{count} of these you filed under {labels} yourself. This rule would label them too.",

  "rules.suggest.title": "Make this automatic?",
  "rules.suggest.body": "You labelled {count} messages {label} by hand. A rule could keep doing it.",
  "rules.suggest.effect": "It would also label {collateral} other messages you have.",
  "rules.suggest.effectNone": "It matches nothing else you have — only mail like these.",
  "rules.suggest.review": "Review rule",
  "rules.suggest.defaultName": "{label} (suggested)",

  // Running a saved rule over mail you already have (#191). The blast radius is
  // shown BEFORE anything is written, and the result names what it did NOT do —
  // a backfill that quietly performed three quarters of a rule would be worse
  // than one that refused outright.
  "rules.apply.button": "Apply to existing mail",
  "rules.apply.title": "Apply “{name}” to mail you already have",
  "rules.apply.checking": "Checking what this matches…",
  "rules.apply.matches": "It matches {matched} of your {total} messages.",
  "rules.apply.matchesBounded":
    "It matches {matched} of the {scanned} most recent messages (you have {total}). Older mail was not checked.",
  "rules.apply.none": "It matches nothing you already have — only mail that arrives from now on.",
  "rules.apply.confirm": "Label these {count}",
  "rules.apply.applying": "Applying…",
  "rules.apply.done": "Labelled {count} messages.",
  "rules.apply.nothingToDo": "Nothing to do — they already had it.",
  "rules.apply.alreadyDone": "{count} already had it.",
  "rules.apply.skippedCorrected":
    "{count} left alone — you had taken this rule's label off them, and putting it back would undo your own correction.",
  "rules.apply.skippedActions":
    "Not applied to old mail: {actions}. Those decide where NEW mail lands, and running them over what you have already sorted would move it.",
  "rules.apply.truncated":
    "Stopped after {changed}; {remaining} more match. Run it again to continue.",
  "rules.apply.undo": "Undo",
  "rules.apply.undone": "Undone — {count} messages put back.",
  "rules.apply.failed": "Could not apply the rule.",
  "rules.apply.disabledHint": "Turn the rule on to apply it to mail you already have.",
  // A rule that files into an AREA (#708): applying it moves mail, and mail
  // you (or another rule) filed elsewhere stays put. Counted per outcome.
  "rules.apply.area.willMove": "{count} will move to {area}.",
  "rules.apply.area.alreadyThere": "{count} already in {area}.",
  "rules.apply.area.confirm": "Move {count} to {area}",
  "rules.apply.area.moved": "Moved {count} to {area}.",
  "rules.apply.area.contested": "{count} stay where they are: {reasons}.",
  "rules.apply.area.byUser": "{count} you filed in another area yourself",
  "rules.apply.area.byRule": "{count} another rule filed elsewhere",
  "rules.apply.area.byOther": "{count} a folder label or the classifier sorted elsewhere",

  // A misfiring rule, and the offer to fix it (#170). The wording is about the
  // RULE, never about the user: "this rule has misfiled 4" states what happened,
  // where "you corrected this rule 4 times" would read as a scolding for
  // disagreeing with automation.
  "rules.corrections.badge": "misfiled {count}",
  "rules.corrections.one": "You have taken this rule's label back off 1 message.",
  "rules.corrections.many": "You have taken this rule's label back off {count} messages.",
  "rules.corrections.narrow": "Narrow it",
  "rules.corrections.checking": "Working out a narrower rule…",
  "rules.corrections.title": "Narrow “{name}”",
  "rules.corrections.current":
    "As it stands it matches {matched} messages: {keeps} you kept, {misfires} you took the label off, {collateral} nobody has judged.",
  "rules.corrections.proposal":
    "This narrower rule matches {matched}: it keeps {keeps} and no longer catches the {fixes} you rejected.",
  "rules.corrections.drops": "It would also stop labelling {count} messages you had kept:",
  "rules.corrections.dropsNone": "It stops labelling nothing you had kept.",
  "rules.corrections.bounded":
    "Measured over the {scanned} most recent messages (you have {total}). Older mail was not checked.",
  "rules.corrections.review": "Review and save",
  "rules.corrections.none":
    "No narrower rule fits. The mail you rejected looks the same as the mail you kept — same sender, same list — so nothing a rule can test tells them apart. Editing it by hand or turning it off are the remaining options.",
  "rules.corrections.outOfWindow":
    "The messages this rule got wrong are older than we can check, so a narrower rule cannot be verified against them.",
  "rules.corrections.failed": "Could not work out a narrower rule.",

  "rules.fromMessage": "Create a rule from this message…",
  "rules.fromMessage.sender": "From this sender ({from})",
  "rules.fromMessage.domain": "From anyone at {domain}",
  "rules.fromMessage.subject": "With this subject",
  "rules.fromMessage.list": "From this mailing list",
  "rules.fromMessage.defaultName": "Mail from {who}",

  // A rule nobody named names itself after BOTH halves of what it does (#292).
  // The sender alone was the old default and it collided with every rule the
  // user already had for that sender, because rule names are unique per user.
  "rules.autoName": "{key} → {label}",

  // You are about to write a rule you already have (#292). Two different
  // findings, two different sentences: the same conditions with the same label
  // is a rule that would do nothing new, while the same conditions with a
  // DIFFERENT label is mail that starts arriving with two labels on it. Both are
  // worth stopping for; only the first is redundant.
  "rules.duplicate.same": "You already have this rule.",
  "rules.duplicate.sameBody":
    "It matches the same mail and applies the same label. A second copy would not label anything new — it would just run on every message alongside the first.",
  "rules.duplicate.other": "You already have a rule for this mail.",
  "rules.duplicate.otherBody":
    "It matches the same mail and files it somewhere else. If you create this one too, mail like this will arrive with both labels.",
  "rules.duplicate.edit": "Edit that rule instead",
  "rules.duplicate.createAnyway": "Create a second rule anyway",

  "rules.appliedBy.rule": "Labelled by the rule “{name}”",
  "rules.appliedBy.deletedRule": "Labelled by a rule that no longer exists",
  "rules.appliedBy.ml": "Labelled automatically",
  "rules.appliedBy.agent": "Labelled by an agent",

  "shortcuts.labelFast": "Apply one of the first nine labels without leaving the keyboard",
  "shortcuts.undo": "Undo the last thing you did",

  "labels.filter": "Filter labels",
  "labels.applyHint": "Press 1–9 to apply a label, or type to filter.",
  // A local-only label (#704, ADR-031) — every area's own label is one. It is
  // applied like any other; the hint only says where it lives, so nobody
  // expects to see it in another mail app.
  "labels.localOnly": "stays in Mailroom",

  // ---- The desk (#375) ----------------------------------------------------
  //
  // The desk is a screen that STATES ITS OWN NUMBERS, so most of these are
  // sentences with counts in them rather than labels. Plural forms are written
  // as two keys where English needs them (`desk.tray.overWeek.one/other`) —
  // this app has no ICU library on purpose (see the file header), and a
  // catalogue that pretends one form fits every language is worse than one that
  // is honestly two keys.
  "desk.pane": "The desk",
  "desk.header.inbox": "Inbox",
  "desk.header.allAccounts": "all accounts",
  "desk.header.loading": "loading…",
  "desk.header.inTray": "{n} in tray",
  "desk.header.toReview": "{n} to review",
  "desk.header.inDrawers": "{n} in drawers",
  // Appended to the header sentence rather than replacing the screen: the
  // numbers stay visible next to the reason they may be stale.
  "desk.header.offline": "offline, showing what was loaded",
  "desk.header.unreachable": "cannot reach the server",
  // "synced …" is never a bare clock: the question is "is this current", and
  // the answer to that is a duration, not a time of day.
  "desk.synced.never": "not synced yet",
  "desk.synced.justNow": "synced just now",
  "desk.synced.seconds": "synced {n} s ago",
  "desk.synced.minutes": "synced {n} min ago",
  "desk.synced.hours": "synced {n} h ago",
  "desk.synced.days": "synced {n} d ago",

  "desk.tray.title": "In-tray",
  "desk.tray.empty": "Nothing in the tray.",
  "desk.tray.oldest": "oldest waiting {wait}",
  // The tray loads a bounded page, so when it did not all fit the sentence is
  // about the SAMPLE, not about the tray.
  "desk.tray.oldestBounded": "oldest waiting {wait} among the {loaded} loaded",
  "desk.tray.overWeek.one": "1 over a week",
  "desk.tray.overWeek.other": "{n} over a week",
  "desk.tray.open": "Open tray",

  // FOLLOW UP (#58): the nudges on the desk (ui/desk/FollowUpStrip.tsx).
  "followUp.title": "Follow up",
  "followUp.outbound": "No one answered your mail to {who} from {day}",
  "followUp.outboundNoWho": "No one answered your mail from {day}",
  "followUp.inbound.one": "You haven't replied to {who} in {count} day",
  "followUp.inbound.other": "You haven't replied to {who} in {count} days",
  "followUp.inboundNoWho.one": "You haven't replied to this in {count} day",
  "followUp.inboundNoWho.other": "You haven't replied to this in {count} days",
  "followUp.noSubject": "(no subject)",
  "followUp.done": "Done",
  "followUp.snooze": "Snooze…",
  "followUp.doneFailed": "Could not mark that follow-up done",
  "desk.wait.underHour": "under an hour",
  "desk.wait.hours": "{n} h",
  "desk.wait.days": "{n} d",

  "desk.tabs.label": "Inbox halves",
  // The tab's own accessible name (#447), which the list borrows: the visible tab
  // is a label and a count pill, and "6" on its own is a number with no noun.
  "desk.tab.name": "{tab}, {n} messages",
  "desk.tab.review": "Review first",
  "desk.tab.rest": "Everything else",

  "desk.section.needsSorting": "Needs sorting",
  // Why this pile is worth clearing: placing a message once is what creates the
  // label and the rule that place the next one.
  "desk.section.needsSortingHint": "place once → label + rule",
  "desk.section.more": "Show {n} more of {total}",
  "desk.section.error": "This part of the desk could not be loaded: {error}",

  "desk.line.label": "The line",
  "desk.line.toReview": "{n} to review",
  "desk.line.inDrawers": "{n} in drawers",
  "desk.line.stricter": "stricter",
  "desk.line.looser": "looser",
  "desk.line.preview": "at {threshold}: {review} to review · {drawers} in drawers",
  "desk.line.previewing": "Working out what that would show…",
  "desk.line.previewFailed": "Could not work out what that would show.",
  "desk.line.apply": "Apply",
  // ---- what the line says out loud (#447) ---------------------------------
  //
  // The line is a slider whose value is a triage threshold between 0 and 1, and
  // "0.55" is the least meaningful thing about it — what the number DOES is split
  // the mail, so that split is the value text. The third clause names the PENDING
  // threshold rather than "one step", because the line can be several steps from
  // where it started and a slider that misdescribes what it is about to do is
  // worse than one that says less (ui/desk/format.ts).
  "desk.line.value": "{review} to review, {drawers} in drawers",
  "desk.line.moveToDrawers": "the line at {threshold} would move {n} to the drawers",
  "desk.line.moveToSurface": "the line at {threshold} would move {n} to the surface",
  "desk.line.moveNone": "the line at {threshold} would move nothing",

  "desk.drawer.open": "Open",
  "desk.drawer.close": "Close",
  "desk.drawer.archiveAll": "Archive all",
  // Starts with the visible words "Archive all" (WCAG 2.5.3, axe
  // label-content-name-mismatch from axe-core 4.14): "Archive everything in"
  // does not contain what the button shows.
  "desk.drawer.archiveAllNamed": "Archive all in {area}",
  "desk.drawer.sweep": "auto-archives after {n} d",
  "desk.drawer.noSweep": "never auto-archives",
  // The period is a setting; whether the sweep RUNS is a separate, login-level
  // switch that defaults to off (#443). Both are said, because printing only
  // the period would promise something nothing performs.
  "desk.drawer.sweepOff": "sweep off",
  // ---- the drawer's button, named (#447) -----------------------------------
  //
  // Its visible word is "Open"/"Close" and its NAME is the row: a reader tabbing
  // down this tab used to hear "Open, button" once per area with nothing to tell
  // them apart. The period is spelled out here and abbreviated on screen ("30
  // days" against "30 d") — the row has a column to fit, and "d" read aloud is
  // the letter d.
  "desk.drawer.button.one": "{area} drawer, 1 message, {sweep}",
  "desk.drawer.button.other": "{area} drawer, {count} messages, {sweep}",
  "desk.drawer.sweepDays.one": "auto-archives after 1 day",
  "desk.drawer.sweepDays.other": "auto-archives after {count} days",
  "desk.drawer.confirmTitle": "Archive {n} messages in {area}?",
  "desk.drawer.confirmBody": "They stay under their labels, in All mail and in search.",
  "desk.drawer.archived": "Archived {n} messages from {area}",
  // Archive-all is capped, and it says so rather than presenting a partial
  // sweep as a finished one.
  "desk.drawer.archivedBound": "Archived {n} of {total} messages from {area} — run again for the rest",
  "desk.drawer.archivedFailed": "{n} could not be archived",

  "desk.row.select": "Select mail from {sender}",
  // The main button OPENS. It used to be named "{sender}: {subject}", which put
  // the sender in the announcement twice now that the row itself is named
  // (ui/desk/rowName.ts): the row is the sentence, the buttons are what you can
  // do to it.
  "desk.row.open": "Open {subject}",
  // With a snippet showing, the name has to carry it too: the button's visible
  // text is subject + snippet, and a name that leaves the snippet out does not
  // contain what a voice-control user sees (WCAG 2.5.3, axe
  // label-content-name-mismatch from axe-core 4.14).
  "desk.row.openWithPreview": "Open {subject} – {preview}",
  "desk.row.alias": "arrived at {alias}",
  "desk.row.thread": "{n} messages in this conversation",
  "desk.row.flagged": "In the tray",
  // ---- the row's name in words (#447) -------------------------------------
  //
  // Every one of these is a fact the SCREEN states with a glyph, a weight or a
  // colour, and the row's accessible name states with a word. "Read" has no key
  // on purpose: the absence of "Unread" is the statement, and saying it on nine
  // rows in ten would pad every announcement with the least interesting thing
  // about the row.
  "desk.row.unread": "Unread",
  "desk.row.inArea": "in {area}",
  // The row draws the account as a coloured dot. This is the same fact in a word,
  // on exactly the rows that draw one.
  "desk.row.account": "{account} account",
  "desk.row.pinned": "Pinned",
  "desk.row.returned": "Came back from a snooze",
  "desk.row.heldAttachment": "An attachment is held",
  // The row's time bucket. A bare clock on yesterday's mail reads as "this
  // morning", which is the classic mail-client lie; the word is the answer.
  "desk.time.yesterday": "Yest.",

  "desk.selection.label": "What to do with the selection",
  "desk.selection.count": "{n} selected",
  "desk.selection.tray": "In-tray",
  "desk.selection.markRead": "Mark read",
  "desk.selection.markUnread": "Mark unread",
  "desk.selection.refile": "Re-file",
  "desk.selection.clear": "Clear",
  "desk.refile.label": "Re-file to an area",
  "desk.refile.noAreas": "You have no areas yet.",
  // An area owns a label, so re-filing IS applying it (#704: local-only labels
  // included). The only remaining block is the label not having loaded yet.
  "desk.refile.labelMissing": "{area}'s label has not loaded yet, so mail cannot be filed there right now.",

  "desk.empty": "Nothing in the tray. Nothing to review. {n} in drawers.",

  // The rail and switcher (#375). The rail's entries are DESTINATIONS, so they
  // are nouns; the switcher's words are STATES, so they are adjectives. Where a
  // string already existed for the same destination (Inbox, Sent, Drafts, All
  // mail, Folders, Labels) it is reused rather than duplicated under a second
  // key — two catalog entries for one word is two things to translate and one
  // of them to get out of step.
  "rail.tray": "In-tray",
  "rail.outbox": "Outbox",

  // ---- the Outbox (#448, design pass-2 §8) --------------------------------
  //
  // TWO WORDS FOR ONE SERVER CALL, and the difference is what the reader is doing:
  // inside the hold window they are TAKING BACK something already going (Undo);
  // outside it they are CALLING OFF something arranged (Cancel). Both are
  // `undoStatus: "canceled"`, the only legal EmailSubmission update.
  "outbox.title": "Outbox",
  "outbox.pane": "Outbox",
  "outbox.rows": "Messages waiting to send",
  "outbox.empty": "Nothing else waiting. Once a message has gone it is in Sent.",
  "outbox.sendingIn": "Sending in {seconds} s",
  "outbox.scheduled": "Scheduled · {when}",
  // No number, deliberately: the moment has passed and the only honest thing to
  // say is that it is on its way. A countdown here would tick through zero and
  // stick (#448), which is why these rows used to be hidden altogether (#589).
  "outbox.sending": "Sending…",
  // ⚠ SAYS WHAT DID NOT HAPPEN, not "error". The user's mental model after the
  // undo countdown ends is that the message went; the sentence has to undo that
  // belief before anything else. "Not sent" does; "Send failed" invites reading
  // it as a transient hiccup, which this never is — anything retryable is
  // retried and never reaches this state (compose/send.go).
  "outbox.failed": "Not sent — the mail server refused it",
  // The provider's verbatim reply. It is the only thing that makes the cause
  // actionable: "sender address rejected" told the operator it was the FROM
  // address, not the recipient, the network, or us.
  "outbox.failedReply": "The server said: {reply}",
  // ⚠ NAMES THE RECOVERY, because the state the user is left in is misleading on
  // its own: the message is still in Drafts, which reads as "I never sent it"
  // rather than "it was refused", and the obvious move is to send it again
  // unchanged — which fails again identically. Saying where it is and that it
  // needs changing first is what stops that loop.
  "outbox.failedNoReply": "The server gave no reason.",
  "outbox.failedNext": "It is still in Drafts. Change what the server objected to before sending again.",
  // The action that sentence asks for, which the row did not offer until #604.
  "outbox.failedEdit": "Edit the draft",
  // Taking a failure off the Outbox without re-sending it. Nothing is deleted:
  // the draft stays in Drafts, which the toast says so nobody thinks it went.
  "outbox.dismiss": "Dismiss",
  "outbox.dismissed": "Removed from the Outbox. The draft is still in Drafts.",
  "outbox.as": "as {alias}",
  "outbox.undo": "Undo",
  "outbox.cancel": "Cancel",
  "outbox.sendNow": "Send now",
  "outbox.sendingNow": "Sending now.",
  "outbox.noSubject": "(no subject)",
  // A submission whose recipients the server has not been asked for yet is a row
  // that exists with nothing to name — never a hidden row, which would
  // under-report what is waiting.
  "outbox.noRecipients": "no recipients",
  // "Areas · review / drawer" — the heading states what the two numbers beside
  // each area MEAN, once, instead of every row carrying a legend.
  "rail.areas": "Areas · review / drawer",
  "rail.needsSorting": "Needs sorting",
  "rail.settings": "Settings",

  "switcher.allAccounts": "All accounts",
  "switcher.label": "Choose an account",
  "switcher.health": "Account health",
  // The trailing clause on the folded line: how many accounts are not ok. One
  // kind at a time, worst first — adding a paused account to a failing one
  // would give a number that describes neither.
  "switcher.paused": "{n} paused",
  "switcher.failing": "{n} failing",
  // Beside one account in the open list. Nothing is said about a healthy one.
  "switcher.statusPaused": "paused",
  "switcher.statusFailing": "failing",

  "tray.title": "In-tray",
  "tray.pane": "The in-tray",
  "tray.list.label": "Everything in your tray",
  // The window is a SAMPLE past the first page, and the sentence says so —
  // which is also what keeps "oldest waiting" honest, since a bounded window
  // holds the newest rows and therefore the least old of them.
  "tray.bounded": "showing the newest {n} of {total}",
  "tray.remove": "Remove from tray",
  // The four groups, in order. They are durations, not dates: the question is
  // "what have I been ignoring", and the answer to that is a shape.
  "tray.group.today": "Today",
  "tray.group.thisWeek": "This week",
  "tray.group.overWeek": "Over a week",
  "tray.group.overMonth": "Over a month",

  // The reading pane (#375). Words, never key legends: the Keys sheet is the
  // one place a shortcut is printed.
  "pane.toolbar": "What to do with this message",
  "pane.more": "More",
  "pane.more.label": "More things to do with this message",
  // "Label" is the desk's word for the verb; the ellipsis is the promise that
  // a dialog follows, which is what tells it apart from the chips beside it.
  "pane.menu.label": "Label…",
  // "Re-file" is the desk's word for moving mail between areas (#921), and the
  // ellipsis is the same promise that a dialog follows.
  "pane.menu.refile": "Re-file…",
  // The picker the pane's item opens: one area per item, the answer is a move.
  "refile.picker.title": "Re-file to…",
  "pane.labels": "Where this message is filed",
  "pane.addLabel": "+ label",
  // "Area: Personal — by rule". The second half names WHO filed it, because
  // "why is this here" is the question an area chip most often provokes.
  "pane.area.title": "Area: {area} — {how}",
  "pane.area.by.user": "you placed it",
  "pane.area.by.rule": "by rule",
  "pane.area.by.desk": "by the desk's plan",
  // #714: learned placement filed it, from the user's own earlier Re-files of
  // this sender. Says WHOSE evidence it was, because "why is this here" is the
  // question, and the answer is "because you put this sender's mail here".
  "pane.area.by.learned": "learned from your moves",
  // An API key filed it (#49, K2 records areaProvenance "agent").
  "pane.area.by.agent": "filed by an AI tool (API key)",
  "pane.area.by.auto": "automatically",
  "pane.label.title": "Label: {label}",
  // Email.lineSide (#464): the user's own placement relative to the line,
  // shown only when they made one — see ui/pane/lineSide.ts.
  "pane.lineSide.surface": "You placed this above the line",
  "pane.lineSide.drawer": "You placed this in the drawer",

  // The sender popover (#375). Every line is omitted when its fact is unknown,
  // so none of these is ever printed with nothing after it.
  "popover.aboutSender": "About {sender}",
  "popover.writesTo": "writes to {alias}",
  "popover.whyHere": "Why it is here",
  "popover.whyHere.up": "Ranked higher: {reasons}",
  "popover.whyHere.down": "Ranked lower: {reasons}",
  // "Seal" is the state of the message's authentication and, later, its
  // cryptographic signature — one heading for "what do we know about who sent
  // this", rather than two the reader has to tell apart.
  "popover.seal": "Seal",
  "popover.area": "Area",
  "popover.areaLine": "{area}, {how}",
  "popover.pin": "Keep this open",
  "popover.unpin": "Close",
  "popover.alwaysShowImages": "Always show images from {address}",
  "popover.stopShowingImages": "Stop always showing images",
  "popover.changeArea": "Change area",

  // Moving many messages into an area at once (#708): from the list's selection
  // toolbar, and "everything from this sender" on the sender card. Every count
  // is said per case, and a capped selection says so rather than reading as done.
  "areaMove.menu": "Move to area",
  "areaMove.menuLabel": "Move the selected messages to an area",
  "areaMove.moved.one": "Moved 1 message to {area}.",
  "areaMove.moved.other": "Moved {n} messages to {area}.",
  "areaMove.partial": "Moved {moved} of {total} messages to {area}.",
  "areaMove.nothing": "Nothing to move to {area}.",
  "areaMove.alreadyThere": "{n} already in {area}.",
  "areaMove.unreadable": "{n} could not be read and were skipped.",
  "areaMove.notTouched": "{n} more were not touched.",
  "areaMove.readFailed": "Could not read those messages, so nothing was moved.",
  "areaMove.undone": "Move undone",
  "areaMove.sender.button": "Move all from {address} to…",
  "areaMove.sender.menuLabel": "Move every message from {address} to an area",
  "areaMove.sender.finding": "Counting messages from {address}…",
  "areaMove.sender.none": "There are no messages from {address} to move.",
  "areaMove.sender.failed": "Could not count the messages from {address}.",
  "areaMove.sender.noSearch":
    "Search is turned off on this server, so Mailroom cannot find every message from {address}.",
  "areaMove.sender.confirm.one": "Move the 1 message from {address} to {area}?",
  "areaMove.sender.confirm.other": "Move all {n} messages from {address} to {area}?",
  "areaMove.sender.confirm.capped":
    "{address} has {total} messages. Move the first {n} to {area}? The rest stay where they are until you move them too.",
  "areaMove.sender.learn":
    "Future mail from this sender follows once Mailroom has learned it, except mail that cannot be authenticated, which needs a rule.",
  "areaMove.sender.go": "Move to {area}",
  "areaMove.sender.rest": "Move the rest ({n})",

  // ---- Senders: VIP and block (#59) ----------------------------------------
  "senderBulk.archive": "Archive",
  "senderBulk.junk": "Junk",
  "senderBulk.trash": "Trash",
  "senderBulk.notSettled":
    "The last move has not reached the server yet, so the rest cannot be counted. Try again when you are back online.",
  // A toggle: the NAME stays the same and aria-pressed carries the state.
  "senders.vip.toggle": "VIP",
  "senders.vip.failed": "Could not change whether this sender is a VIP",
  "senders.block.open": "Block {address}…",
  "senders.block.formLabel": "Block {address}",
  "senders.block.where": "New mail from {address} goes to",
  "senders.block.toJunk": "Junk",
  "senders.block.toTrash": "Trash (deleted after 30 days)",
  "senders.block.moveExisting": "Also move the mail already here from {address}",
  "senders.block.reversible":
    "Matched by this exact address. You can unblock from this card or in Settings › Senders.",
  "senders.block.go": "Block",
  "senders.block.failed": "Could not block this sender",
  "senders.blocked.junk": "Blocked {address}. New mail from them goes to Junk.",
  "senders.blocked.trash": "Blocked {address}. New mail from them goes to Trash.",
  "senders.unblock": "Unblock {address}",
  "senders.unblock.failed": "Could not unblock this sender",
  "senders.unblocked": "Unblocked {address}. New mail from them arrives as usual.",
  "senders.isBlocked.junk": "Blocked: new mail from this address goes to Junk.",
  "senders.isBlocked.trash": "Blocked: new mail from this address goes to Trash.",


  // ---- Undo and the header states (#375) ----------------------------------
  //
  // TWO KEYS WHERE ENGLISH NEEDS TWO, as everywhere else in this catalogue
  // (there is no ICU library here on purpose — see the file header).
  //
  // The unit is the MESSAGE, not the conversation: every one of these verbs
  // acts on the ids a row stands for (core/actions.ts takes `ids: Id[]` of
  // Emails), and the existing drawer wording already says "messages". A toast
  // that said "3 conversations" after archiving three rows of one thread would
  // be the client counting something it did not do.
  "undo.archived.one": "Archived 1 message",
  "undo.archived.other": "Archived {n} messages",
  "undo.trashed.one": "Moved 1 message to Trash",
  "undo.trashed.other": "Moved {n} messages to Trash",
  // ⚠ SAYS ARCHIVE, NOT INBOX, AND THAT IS THE POINT. The server restores a
  // trashed message to Archive (internal/store Restore ⇒ MoveToRole
  // "archive"), so this is what actually happened — "Back in your inbox" would
  // be a sentence about a place the message is not.
  "undo.restoredToArchive.one": "Restored 1 message to Archive",
  "undo.restoredToArchive.other": "Restored {n} messages to Archive",
  // "Not junk" (#528) names where the message went, because the row simply
  // leaving the Junk list would read as "it was deleted". Not an undo offer:
  // the inverse would be marking it as junk (core/actions.ts notJunk).
  "undo.notJunk.one": "Moved 1 message to Inbox",
  "undo.notJunk.other": "Moved {n} messages to Inbox",
  "undo.filed.one": "Filed 1 message in {area}",
  "undo.filed.other": "Filed {n} messages in {area}",
  // The desk's word for the flag is the TRAY; the message list's is the star.
  // Same keyword underneath, two vocabularies on top, and a toast that used the
  // other surface's word would be the app renaming things as you move around it.
  "undo.trayed.one": "Put 1 message in the tray",
  "undo.trayed.other": "Put {n} messages in the tray",
  "undo.untrayed.one": "Took 1 message out of the tray",
  "undo.untrayed.other": "Took {n} messages out of the tray",
  "undo.starred.one": "Starred 1 message",
  "undo.starred.other": "Starred {n} messages",
  "undo.unstarred.one": "Unstarred 1 message",
  "undo.unstarred.other": "Unstarred {n} messages",
  "undo.snoozed.one": "Snoozed 1 message",
  "undo.snoozed.other": "Snoozed {n} messages",
  "undo.read.one": "Marked 1 message read",
  "undo.read.other": "Marked {n} messages read",
  "undo.unread.one": "Marked 1 message unread",
  "undo.unread.other": "Marked {n} messages unread",
  // A BULK importance correction (#921) says how many CHANGED, not how many
  // were selected: rows that already carried the value did not move.
  "undo.markedImportant.one": "Marked 1 as important",
  "undo.markedImportant.other": "Marked {n} as important",
  "undo.markedNotImportant.one": "Marked 1 as not important",
  "undo.markedNotImportant.other": "Marked {n} as not important",
  // The countdown. Muted type beside the button, and its own spoken form —
  // "8 s" alone tells a screen reader nothing about what is running out.
  "undo.secondsLeft": "{n} s",
  "undo.timeLeft": "{n} seconds left to undo",
  "undo.dismiss": "Dismiss",

  // Offline: this browser cannot reach Mailroom. The sentence states what is on
  // screen, how old it is, and how much is waiting — in that order, because
  // that is the order the questions arrive in.
  "header.offline.lead": "Offline. Showing what was loaded",
  "header.offline.lastSynced": "last synced {duration} ago",
  // Not "synced 0 s ago": nothing has ever come back on this device.
  "header.offline.neverSynced": "never synced on this device",
  "header.offline.queued.one": "1 change waiting to send",
  "header.offline.queued.other": "{n} changes waiting to send",
  "header.offline.retry": "Retry",
  // The strip's shorter form: the counts beside it were true at that moment.
  "header.offline.asOf": "as of {duration} ago",

  // Stale: Mailroom is answering; it is MAILROOM that cannot reach the mail
  // provider. Every clause after the first is printed only when the health data
  // actually carries it (core/headerState.ts).
  "header.stale.lead": "Sync is behind.",
  "header.stale.lastSynced": "{account} last synced {duration} ago",
  "header.stale.neverSynced": "{account} has never synced",
  "header.stale.since": "provider not reachable since {time}",
  "header.stale.retry": "retrying every {duration}",
  "header.stale.details": "Details",
  // The reassurance that makes the warning actionable rather than alarming:
  // nothing the user does here is at risk, so nothing has to be redone later.
  "header.stale.note":
    "Everything you do is saved here and reaches the provider when it is reachable again.",

  // Ingest-only mode's persistent mail-view banner (ADR-028 §5, #521, core/
  // writebackBanner.ts). Deliberately SEPARATE from offline/stale above: a
  // shadowed account can be perfectly in sync, so this must say so regardless
  // of connection health rather than compete with those for one slot. It
  // states the actual contract, not a promise the queue drains on lift — it
  // does not, until an explicit replay (ADR-028 §4) — so nothing here reads
  // as "will be applied later".
  "shadow.title": "{name} is ingest-only",
  "shadow.notWriting": "Mailroom is not writing to this server.",
  "shadow.flagsProvisional":
    "Read/unread and stars are provisional — they mirror the server and will be replaced by it.",
  "shadow.localDurable": "Labels, filing and trash stay local and are durable.",
  "shadow.suppressed.one": "1 change recorded but not sent.",
  "shadow.suppressed.other": "{count} changes recorded but not sent.",

  // Durations, in the units the sentence needs them. Two-part forms exist
  // because "3 h" for something three hours and fifty minutes behind reads as
  // an understatement of nearly an hour.
  "duration.seconds": "{n} s",
  "duration.minutes": "{n} min",
  "duration.hours": "{n} h",
  "duration.hoursMinutes": "{h} h {m} min",
  "duration.days": "{n} d",
  "duration.daysHours": "{d} d {h} h",

  // On a row. Both are muted tags rather than sentences: they qualify one row,
  // and the header above already carries the reason.
  "row.waitingToSend": "waiting to send",
  // The section said this id was there and the store has no message for it —
  // which is what "showing what was loaded" means one row at a time.
  "row.notLoaded": "not loaded",

  // ---- The phone (#375 increment 7) ---------------------------------------
  //
  // Below 760px the desk is one column, and the words below are the ones that
  // exist only there. Everything else on that screen reuses the catalogue it
  // already had: the offline and stale sentences are UNCHANGED in wording
  // (they are the same failure and the same explanation on any screen), the
  // in-tray line is the same clauses the strip states, and the bottom bar's
  // five destinations are the rail's own words.
  "bottomBar.label": "Where to go",
  // Short on purpose: five labels share the width of a phone. The header's
  // field is what this focuses, so it says the same thing that field's label
  // does, in one word.
  "bottomBar.search": "Search",
  // "3 of 6" on the reading pane, where the pane has REPLACED the list and this
  // is the only thing on screen saying how much is left. One-based, because it
  // is a sentence a person reads (ui/desk/flatten.ts positionOf).
  "pane.position": "{index} of {count}",

  // Crossing the line by hand (#464). The desk's own vocabulary: above the line
  // is the SURFACE (what you are being shown), below it is a DRAWER (what the
  // line kept). Two keys per direction because English needs two.
  "undo.toSurface.one": "Moved 1 message above the line",
  "undo.toSurface.other": "Moved {n} messages above the line",
  "undo.toDrawer.one": "Moved 1 message below the line",
  "undo.toDrawer.other": "Moved {n} messages below the line",
  "shortcuts.toSurface": "Move above the line — this belongs on the surface",
  "shortcuts.toDrawer": "Move below the line — this belongs in the drawer",

  // Whether a message opens BESIDE the list or replaces it (#375). Synced per
  // client family, unlike the orientation above it, which is this device's.
  "settings.messageOpens": "Message opens",
  "settings.messageOpens.beside": "Beside the list",
  "settings.messageOpens.focused": "Focused",
  "settings.messageOpens.hint":
    "Synced to your other browsers. The orientation above stays with this device.",

  // ---- day-one setup (#446) ------------------------------------------------
  //
  // THE WORDING IS THE DESIGN'S OWN and is kept verbatim where the design wrote a
  // sentence. It is doing more work than usual: this wizard is where a person
  // decides what happens to every message they have, and each sentence has to say
  // what will change AND what will not — "nothing moves on the server" appears
  // three times because it is the promise the whole feature rests on.
  //
  // Every number in these strings is a parameter filled from a server count. There
  // is no key here that states a quantity of its own.
  "setup.title": "Set up your desk",
  "setup.steps": "Setup steps",
  "setup.step": "Step {n} of 4",
  "setup.step.1": "What is in your mailbox",
  "setup.step.2": "The mail already in those folders",
  "setup.step.3": "Rules, learned from your folders",
  "setup.step.4": "The line and the drawers, to start",
  "setup.skip": "Skip setup, open the desk",
  "setup.back": "Back",
  "setup.next.2": "Next: what to do with the mail already there",
  // Step 2's primary is "Do it, then next" until the import has been started; once
  // it is done, the same position carries the plain way on.
  "setup.next.3": "Next: rules, learned from your folders",
  "setup.next.4": "Next: the line and the drawers",
  "setup.finish": "Open the desk",
  "setup.loading": "Looking at your folders…",
  // Not "you have no folders": a census that could not be taken and a mailbox with
  // nothing in it are different facts, and only one of them is a reason to go on.
  "setup.failed":
    "Your folders could not be read, so there is nothing to propose yet. Try again, or skip setup and open the desk.",
  "setup.reload": "Try again",

  // Step 1
  "setup.census.found":
    "Found {messages} in {folders}. Special folders (Sent, Drafts, Archive, Junk, Trash) are handled for you. The rest look like this:",
  "setup.census.inboxOnly":
    "Found {messages} in INBOX and no other folders. Special folders (Sent, Drafts, Archive, Junk, Trash) are handled for you.",
  "setup.census.inboxOnly.explain":
    "There is nothing to feed the desk from, so there is nothing to decide here. Step 2 still applies to the inbox itself.",
  "setup.count.messages.one": "1 message",
  "setup.count.messages.other": "{n} messages",
  "setup.count.folders.one": "1 folder",
  "setup.count.folders.other": "{n} folders",
  "setup.table.caption": "Your folders, and what day-one proposes for each",
  "setup.table.folder": "Folder",
  "setup.table.msgs": "Msgs",
  "setup.table.looksLike": "Looks like",
  "setup.table.area": "Area",
  "setup.table.label": "Label",
  "setup.shape.oneDomain": "one sender domain",
  "setup.shape.fewSenders": "{n} senders cover {live}",
  "setup.shape.bulk": "bulk mail · {senders} senders",
  "setup.shape.mixed": "mixed senders",
  "setup.shape.empty": "empty",
  "setup.census.inWindow": "{n} in the last {days} days",
  "setup.census.byRule": "by rule / classifier",
  "setup.census.noLabel": "—",
  "setup.census.unclassified": "{n} with no sender address",
  "setup.area.none": "No area",
  "setup.area.new": "New area: {name}",
  "setup.area.for": "Area for {folder}",
  "setup.label.for": "Label for {folder}",
  "setup.feed.legend": "How should new mail arrive?",
  "setup.feed.folders": "Folders feed the desk",
  "setup.feed.recommended": "recommended to start",
  "setup.feed.folders.explain":
    "Mail arriving in these folders counts as unactioned and lands in its area, exactly like INBOX. Your provider’s rules keep running. Nothing changes for your other clients. You can switch later.",
  "setup.feed.mailroom": "Let Mailroom sort",
  "setup.feed.mailroom.explain":
    "New mail should arrive in INBOX and Mailroom places it into areas and labels. You would turn the provider’s rules off yourself (Mailroom cannot edit them yet). Other clients see labels as keywords, not folders.",

  // Step 2
  "setup.mail.window": "Treat as newly arrived",
  "setup.mail.window.days": "{n} days",
  "setup.mail.window.landing": "{n} messages would land on the desk",
  "setup.mail.window.landing.unmeasured": "how much would land on the desk has not been counted yet",
  "setup.mail.older": "Everything older",
  "setup.mail.older.explain":
    "Label and archive · gets its area and labels, lives in All mail, nothing moves on the server",
  "setup.mail.older.count": "{n} messages",
  "setup.mail.perFolder": "Per folder",
  "setup.mail.perFolder.explain":
    "Override any folder: label and archive · bring to the desk (moves to INBOX on the server) · leave alone (import later)",
  "setup.action.labelArchive": "Label and archive",
  "setup.action.bringToDesk": "Bring to the desk",
  "setup.action.leave": "Leave alone",
  "setup.action.for": "What to do with {folder}",
  "setup.would.lead": "This would {clauses}.",
  "setup.would.labels": "write {n} labels",
  "setup.would.archived": "archive {n} in Mailroom (nothing moves on the server)",
  "setup.would.moves": "move {n} messages",
  "setup.would.leave": "leave {n} for later",
  "setup.would.minutes.one": "about 1 minute",
  "setup.would.minutes.other": "about {count} minutes",
  "setup.would.underMinute": "under a minute",
  "setup.would.undo": "You can undo the whole step afterwards.",
  "setup.would.measuring": "Working out what this would do…",
  "setup.would.unmeasured":
    "The preview could not be measured, so nothing is claimed about what this would write.",
  "setup.would.unclassified": "{n} of the messages this would touch carry no sender address at all.",
  "setup.doIt": "Do it, then next",
  "setup.run.heading": "The import",
  // ONE SENTENCE PER STATUS rather than a status WORD dropped into a template:
  // "The import is failed" is what that template produces, and a panel reporting a
  // failure is the last place to be writing broken English.
  "setup.run.state.queued": "The import is queued.",
  "setup.run.state.running": "The import is running.",
  "setup.run.state.done": "The import is done.",
  "setup.run.state.failed": "The import failed.",
  "setup.run.state.undoing": "The import is being undone.",
  "setup.run.state.undone": "The import has been undone.",
  "setup.run.labels": "labels {done} / {planned}",
  "setup.run.archived": "archived {done} / {planned}",
  "setup.run.moved": "moved {done} / {planned}",
  "setup.run.left": "left for later {n}",
  // The one sentence in the panel that is about time rather than counts: a
  // bringToDesk message reaches the desk only after the writeback lands and the
  // next sync updates its provenance, so the panel must not promise it is there.
  "setup.run.moveNote": "moved {n} · they appear on the desk after the next sync",
  "setup.run.error": "It failed: {error}",
  "setup.run.errorNoMessage": "It failed, and the server gave no reason.",
  "setup.run.undo": "Undo this step",
  "setup.run.undoExplain":
    "Undo removes only what this import did — a label you have re-applied since survives, and a moved message goes back to the folder it came from.",
  "setup.run.busy": "A setup run is already in progress",
  "setup.run.unmeasured": "The import could not be read, so its progress is unknown.",
  "setup.run.watching": "Checking every couple of seconds while it runs.",
  "setup.import.title": "Import {folder}",
  "setup.import.explain":
    "One folder, through the same step: the report first, and nothing is written until you choose it.",
  "setup.import.missing":
    "That folder is not one day-one setup can act on — it may have gone, or it may be a special folder, which is mapped without asking.",
  "setup.import.done": "Done",

  // Step 3
  "setup.rules.lead":
    "The folders tell Mailroom who sends what. Each proposed rule shows what it would have done to the mail that is already there.",
  "setup.rules.looking": "Asking what your folders imply…",
  "setup.rules.nothing":
    "No folders to learn from — every one of them is being left alone, so there is nothing to induce a rule from.",
  "setup.rules.folder": "{name} folder",
  "setup.rules.into": "→ {area}",
  "setup.rules.placed": "would have placed {covers} / {examples}",
  "setup.rules.short": "{n} left for sorting",
  "setup.rules.bounded": "measured over the {scanned} most recent of {total}",
  "setup.rules.accept": "Accept",
  "setup.rules.acceptN": "Accept {n}",
  "setup.rules.accepted": "Accepted",
  "setup.rules.acceptAll": "Accept all with a preview above 90%",
  "setup.rules.none": "no clean rule · sort by hand later",
  "setup.rules.mixedSenders": "{n} senders, mixed",
  "setup.rules.needMore":
    "{examples} of the {required} examples it takes · not enough to learn from yet",
  "setup.rules.unmeasured": "This folder could not be asked about, so nothing is proposed for it.",
  "setup.rules.and": "and",
  "setup.rules.or": "or",
  "setup.rules.has": "has {field}",
  "setup.rules.leaf": "{field} {op} {value}",

  // Step 4
  "setup.dials.line": "The line",
  "setup.dials.line.value": "{threshold}",
  "setup.dials.line.preview":
    "at {threshold}, {review} of the {newly} newly arrived would be to review",
  "setup.dials.line.previewing": "asking what that line would show…",
  "setup.dials.line.previewFailed": "that line could not be previewed",
  "setup.dials.line.later": "you can move it any time from the desk",
  "setup.dials.drawers": "Drawers auto-archive after",
  "setup.dials.drawers.never": "never",
  "setup.dials.drawers.days": "{n} days",
  "setup.dials.drawers.perArea": "per area later",
  "setup.dials.drawers.nudge": "{name} is {n}: consider {days}",
  "setup.dials.tray": "In-tray",
  "setup.dials.tray.nag": "nag after {n} days",
  "setup.dials.tray.nagLabel": "Nag after, in days",
  "setup.dials.tray.empty": "nothing is in it yet; flag anything you must come back to",
  "setup.deskline.lead": "Your desk will open with: {clauses}.",
  "setup.deskline.tray": "{n} in the tray",
  "setup.deskline.review": "{n} to review",
  "setup.deskline.drawers": "{n} in drawers",
  "setup.deskline.allMail": "{n} in All mail by area and label",
  "setup.deskline.needsSorting": "{n} in Needs sorting when you import {folders}",
  "setup.deskline.needsSortingUnnamed": "{n} in Needs sorting when you import them",

  // ---- the contacts page (#816) ----------------------------------------------
  "rail.contacts": "Contacts",
  "contacts.title": "Contacts",
  "contacts.search": "Search contacts",
  "contacts.filter.label": "Show",
  "contacts.filter.saved": "All saved",
  "contacts.filter.vip": "VIP",
  "contacts.filter.key": "Has a key",
  "contacts.filter.blocked": "Blocked senders",
  "contacts.sort": "Sort by",
  "contacts.sort.name": "Name",
  "contacts.sort.last": "Last mail (newest first)",
  "contacts.loading": "Reading your contacts…",
  "contacts.unavailable": "This server cannot list saved contacts.",
  "contacts.empty.saved":
    "People you send mail to appear here automatically. Add anyone else with “Add to contacts” next to their address.",
  "contacts.empty.vip": "No VIPs yet. Mark one from a contact’s page or a sender’s card.",
  "contacts.empty.key": "None of your saved contacts has a key yet.",
  "contacts.empty.blocked": "Nobody is blocked.",
  "contacts.empty.search": "Nothing here matches “{text}”.",
  "contacts.row.last": "Last mail {date}",
  "contacts.chip.vip": "VIP",
  "contacts.chip.key": "Key",
  "contacts.chip.blocked": "Blocked",
  "contacts.bound.all.one": "1 contact",
  "contacts.bound.all.other": "{count} contacts",
  "contacts.bound.some": "Showing {shown} of {total}",
  "contacts.more": "Show more",
  "contacts.more.busy": "Reading more…",
  "contacts.add.open": "Add a contact by address…",
  "contacts.add.label": "Address to add",
  "contacts.add.submit": "Add to contacts",
  "contacts.add.busy": "Adding…",
  "contacts.add.unknown":
    "There is no mail from or to {address} yet, so there is no one to add. Once mail arrives from them, or you write to them, they can be added.",
  "contacts.added": "{name} is in your contacts.",
  "contacts.open": "Open contact",
  "contacts.openFor": "Open contact for {name}",
  "contacts.back": "Contacts",
  "contacts.page": "Contact",
  "contacts.page.loading": "Reading this contact…",
  "contacts.page.missing": "This contact no longer exists. It may have been merged into another one.",
  "contacts.notSaved": "Not in your contacts. Their details are kept anyway, because they are in your mail.",
  "contacts.addTo": "Add to contacts",
  "contacts.name.edit": "Edit name",
  "contacts.name.label": "Your name for them",
  "contacts.name.hint":
    "Up to 200 characters. Their next email will not change it. Leave it empty to use the name their mail gives.",
  "contacts.name.save": "Save name",
  "contacts.name.saved": "Saved. You call them {name}.",
  "contacts.name.cleared": "Using the name their mail gives.",
  "contacts.name.theirs": "Their mail says “{name}”.",
  "contacts.name.useTheirs": "Use theirs",
  "contacts.addresses": "Addresses",
  "contacts.writesTo": "writes to you at {alias}",
  "contacts.activity": "Mail",
  // `in` counts every message of theirs or that copied them, other than your
  // own (ADR-041 §3): "written" would claim more than the server counts.
  "contacts.counts.out.none": "You haven’t written to them",
  "contacts.counts.out.one": "You’ve written to them once",
  "contacts.counts.out.other": "You’ve written to them {count} times",
  "contacts.counts.in.one": "1 other message from them or copying them",
  "contacts.counts.in.other": "{count} other messages from them or copying them",
  "contacts.firstMail": "First mail {date}",
  "contacts.lastMail": "Last mail {date}",
  "contacts.actionsFor": "What to do about {name}",
  "contacts.vip": "VIP",
  "contacts.vip.on": "{name} is a VIP.",
  "contacts.vip.off": "{name} is no longer a VIP.",
  "contacts.block.open": "Block…",
  "contacts.block.formLabel": "Block {name}",
  "contacts.block.where": "New mail from them goes to",
  "contacts.block.explain":
    "Mail already here stays where it is. Unblock at any time from this page or the Blocked senders list.",
  "contacts.blocked": "Blocked {name}.",
  "contacts.unblock": "Unblock",
  "contacts.unblocked": "Unblocked {name}. New mail from them arrives as usual.",
  "contacts.isBlocked.junk": "Blocked: new mail from them goes to Junk.",
  "contacts.isBlocked.trash": "Blocked: new mail from them goes to Trash.",
  "contacts.remove": "Remove from contacts…",
  "contacts.remove.title": "Remove {name} from your contacts?",
  "contacts.remove.nothingDeleted":
    "Nothing is deleted: their mail, their keys and key history, your notes and any block all stay. They are just no longer listed here.",
  "contacts.remove.staysVip":
    "{name} stays a VIP: mail from them still counts as VIP, and they stay under the VIP filter. Turn VIP off first if you don’t want that.",
  "contacts.remove.again": "Writing to them again adds them back.",
  "contacts.remove.submit": "Remove from contacts",
  "contacts.remove.busy": "Removing…",
  "contacts.removed": "{name} is no longer in your contacts. Nothing was deleted.",
  "contacts.notes": "Notes",
  "contacts.notes.count": "{count} of {max} characters",
  "contacts.notes.over": "{count} of {max} characters: shorten the notes to save them.",
  "contacts.notes.save": "Save notes",
  "contacts.notes.saved": "Notes saved.",
  "contacts.key.heading": "Key",
  "contacts.key.none": "You don’t hold a key for them.",
  "contacts.key.import": "Import a key for them…",
  "contacts.key.historyLabel": "their keys",
  "contacts.recent": "Recent mail",
  "contacts.recent.loading": "Looking for mail with them…",
  "contacts.recent.none": "No mail with them was found.",
  "contacts.recent.cannot": "None of their addresses can be searched for.",
  "contacts.recent.noSubject": "(no subject)",
  "contacts.recent.bound": "The latest {shown} of {total}.",
  "contacts.recent.skipped": "Not searched (the search cannot spell these addresses): {addresses}.",
  "contacts.recent.all": "See all mail with {name}",
  "contacts.merge.open": "Merge with…",
  "contacts.merge.title": "Merge with {name}",
  "contacts.merge.explain":
    "If {name} also writes from another address, merge the two into one contact. Separate undoes it.",
  "contacts.merge.search": "Find a contact or an address",
  "contacts.merge.searchHint":
    "Saved contacts by name or address, or any full address you have mail from or to.",
  "contacts.merge.results": "Matches",
  "contacts.merge.noMatch": "Nothing matches “{text}”.",
  "contacts.merge.unsaved": "not in your contacts",
  "contacts.merge.previewLabel": "What will be combined",
  "contacts.merge.preview": "Merging {from} into {name}",
  "contacts.merge.addresses": "Addresses: {addresses}",
  "contacts.merge.name": "Name: {name}",
  "contacts.merge.keys.one": "Their key moves too",
  "contacts.merge.keys.other": "Their {count} keys move too",
  "contacts.merge.notesJoined": "Both sets of notes are kept, one after the other",
  "contacts.merge.vip": "A VIP",
  "contacts.merge.saved": "In your contacts",
  "contacts.merge.block.junk": "Blocked: the block on {who} carries over, so new mail from every address goes to Junk",
  "contacts.merge.block.trash":
    "Blocked: the block on {who} carries over, so new mail from every address goes to Trash",
  "contacts.merge.block.both": "both",
  "contacts.merge.undo": "Keys stay pinned per address. Separate splits the addresses apart again.",
  "contacts.merge.submit": "Merge",
  "contacts.merge.submitWith": "Merge {from} in",
  "contacts.merge.busy": "Merging…",
  "contacts.merge.done": "Merged {from} into {name}.",
  "contacts.separate.open.button": "Separate…",
  "contacts.separate.title": "Separate addresses from {name}",
  "contacts.separate.explain":
    "Each address you choose becomes its own contact again, with what it had before it was merged, its block included. When every address a merge brought in is split off, this contact goes back to how it was before that merge, keeping anything you changed since.",
  "contacts.separate.legend": "Addresses to split off",
  "contacts.separate.none": "Choose at least one address.",
  "contacts.separate.all": "Leave at least one address on {name}.",
  "contacts.separate.ready": "The chosen addresses will become a separate contact.",
  "contacts.separate.submit": "Separate",
  "contacts.separate.busy": "Separating…",
  "contacts.separate.done": "Split off into a separate contact:",
  "contacts.separate.open": "Open the contact for {address}",
  "contacts.separate.restored": "This contact is back as it was before the merge.",
  "contacts.separate.kept": "Your edits to {fields} were kept.",
  "contacts.separate.kept.and": " and ",
  "contacts.separate.kept.userName": "your name for them",
  "contacts.separate.kept.notes": "notes",
  "contacts.separate.kept.isVip": "VIP",
  "contacts.separate.kept.isSaved": "saved",
  "contacts.separate.kept.isBlocked": "the block",
  "contacts.separate.unchanged":
    "This contact keeps its details as they are. They change back only when every address of a merge is split off.",
  "contacts.separate.toast.one": "Separated into 1 new contact.",
  "contacts.separate.toast.other": "Separated into {count} new contacts.",
  "contacts.error.notFound": "That contact no longer exists. It may have been merged into another one.",
  "contacts.error.userNameTooLong": "A name can be at most 200 characters.",
  "contacts.error.notesTooLong": "Notes can be at most 4,000 characters. Together these would be longer.",
  "contacts.error.vipConflict": "A VIP is always in your contacts. Turn VIP off first.",
  "contacts.error.invalid": "The server refused that change.",
  "contacts.error.stateMismatch": "Your contacts changed while you were choosing. Try again.",
  "contacts.error.unavailable": "This server cannot do that.",
  "contacts.error.failed": "That did not work. Try again in a moment.",
  // The sender card and the composer's chips (#816).
  "contacts.popover.add": "Add to contacts",
  "contacts.popover.adding": "Adding…",
  "contacts.popover.in": "In your contacts",
  "contacts.popover.failed": "Could not add this sender to your contacts",
  "contacts.chipMenu.label": "Contact options for {name}",
  "contacts.chipMenu.open": "Open contact",
  "contacts.chipMenu.add": "Add to contacts",
  "contacts.chipMenu.asking": "Looking them up…",
  "contacts.chipMenu.none": "No mail with this address yet, so there is no contact to open.",
  "contacts.chipMenu.cannot": "This address cannot be looked up right now.",
  "settings.senders.moved":
    "VIPs and blocked senders are now in Contacts, with everything else about each person.",
  // ---- API keys, approvals and AI-tool setup (#49, ADR-042, ADR-044) ----
  "settings.apiKeys": "API keys and AI tools",
  // Connecting a mail account by signing in at its provider (#52, ADR-046).
  "outbox.waitingReconnect": "Waiting for you to reconnect {account}",
  "outbox.waitingReconnectUnnamed": "Waiting for you to reconnect its account",
  "outbox.waitingProvider": "Sign-in to {provider} is temporarily unavailable on this server; it will be sent when that is fixed",
  "outbox.reconnect": "Reconnect",
  "oauth.provider.google": "Google",
  "oauth.provider.microsoft": "Microsoft",
  "oauth.provider.unknown": "your provider",
  "oauth.connect.google": "Connect Gmail",
  "oauth.connect.microsoft": "Connect Outlook",
  "oauth.connect.group": "Connect an account by signing in",
  "oauth.connect.explain": "Sign in at your provider instead of typing a password. Mailroom never sees your password.",
  "oauth.dialog.title.google": "Connect your Gmail account",
  "oauth.dialog.title.microsoft": "Connect your Outlook or Microsoft 365 account",
  "oauth.dialog.access": "Mailroom will be able to read, send, organise and delete all the mail in this account, just as you can. Whoever runs this Mailroom server can technically do the same, so connect an account only if you trust them.",
  "oauth.dialog.google.unverified": "Google will warn that this app is unverified. That is expected: this Mailroom is run privately for a small group, and Google only verifies apps that are published for everyone. To carry on, choose Advanced and then continue.",
  "oauth.dialog.microsoft.imap": "Outlook.com accounts: switch on IMAP first, in Outlook on the web under Settings, Mail, Forwarding and IMAP. It is off until you turn it on, and Mailroom cannot connect without it.",
  "oauth.dialog.microsoft.work": "Work or school accounts: your organisation's administrator may have to approve Mailroom before you can connect.",
  "oauth.dialog.revoke": "You can take the access back at any time, from Mailroom (Disconnect) or from your {provider} account's security settings.",
  "oauth.dialog.continue": "Continue to {provider}",
  "oauth.dialog.working": "Opening {provider}…",
  "oauth.dialog.failed": "Could not start signing in with {provider}: {error}",
  "oauth.dialog.badUrl": "the server did not return a sign-in address",
  // The result the provider's sign-in came back with (?oauth=<code>).
  "oauth.result.connected": "Your {provider} account is connected.",
  "oauth.result.reconnected": "Your {provider} account is reconnected. Syncing and sending carry on where they stopped.",
  "oauth.result.already_connected": "That {provider} account is already connected here. If it has stopped working, reconnect it.",
  "oauth.result.denied": "You cancelled at {provider}, so nothing was connected.",
  "oauth.result.provider_error": "{provider} reported a problem, so nothing was connected. Try again in a few minutes.",
  "oauth.result.invalid_request": "The reply from {provider} was incomplete, so nothing was connected. Try again.",
  "oauth.result.invalid_state": "That sign-in was already used, or did not start here, so nothing was connected. Start again from Settings.",
  "oauth.result.expired": "The sign-in took too long and expired, so nothing was connected. Start again from Settings.",
  "oauth.result.provider_mismatch": "The sign-in came back from a different provider than the one it started with, so it was refused. Start again from Settings.",
  "oauth.result.exchange_failed": "Mailroom could not finish signing in with {provider}. Try again; if it keeps failing, tell the person who runs this server.",
  "oauth.result.identity_invalid": "{provider} did not confirm the account's address, so nothing was connected.",
  "oauth.result.missing_scope": "Mailroom needs access to your mail to sync it. Start again and leave the mail permission ticked.",
  "oauth.result.no_refresh_token": "{provider} did not give lasting access, so Mailroom could not stay connected. Start again; if this happens again, first remove Mailroom from your {provider} account's app permissions.",
  "oauth.result.mailbox_refused": "{provider} signed you in, but the mailbox refused the connection. For an Outlook.com account, switch on IMAP in Outlook on the web (Settings, Mail, Forwarding and IMAP) and try again.",
  "oauth.result.mailbox_unreachable": "Mailroom could not reach the {provider} mail server just now, so nothing was connected. Try again in a few minutes.",
  "oauth.result.wrong_account": "You signed in as a different account from the one being reconnected. Sign in as the original account.",
  "oauth.result.not_configured": "Signing in with {provider} is not set up on this server. Ask the person who runs it.",
  "oauth.result.server_error": "Something went wrong on this server, so nothing was connected. Try again; if it keeps failing, tell the person who runs it.",
  "oauth.result.reconnect": "Reconnect",
  "oauth.result.dismiss": "Dismiss",
  "oauth.result.dismissLabel": "Dismiss this sign-in message",
  // The reconnect banner, across the app.
  "oauth.banner.one": "{name} needs you to sign in again. Its mail is waiting and nothing is lost.",
  "oauth.banner.many": "{count} accounts need you to sign in again. Their mail is waiting and nothing is lost.",
  "oauth.banner.reconnect": "Reconnect",
  "oauth.banner.review": "Review accounts",
  "oauth.banner.dismiss": "Dismiss",
  "oauth.banner.dismissLabel": "Dismiss the reconnect reminder until next time",
  // Settings, Accounts: an account that signs in at its provider.
  "settings.state.acctReconnect": "needs reconnecting",
  "settings.state.acctOperator": "server sign-in problem",
  "settings.state.acctDisconnected": "disconnected",
  "settings.account.signIn": "Sign-in",
  "settings.account.signIn.via": "Signs in with {provider} as {address}",
  "settings.account.signIn.viaNoAddress": "Signs in with {provider}",
  "settings.account.signIn.active": "Working normally.",
  "settings.account.signIn.needsReauth": "{provider} stopped accepting Mailroom's access, so this account needs you to sign in again. Until then nothing syncs, and changes and outgoing mail wait here: nothing is lost.",
  "settings.account.signIn.operatorError": "This server's connection to {provider} is not working. That is a server configuration problem, not something you can fix by signing in again: contact the person who runs this server.",
  "settings.account.signIn.disconnected": "Disconnected. Mailroom has no access to this account; the mail already here stays. Reconnect to sync and send again.",
  "settings.account.signIn.reason": "Reason given: {reason}",
  "settings.account.signIn.reconnect": "Reconnect",
  "settings.account.signIn.disconnect": "Disconnect…",
  "settings.account.signIn.noServer": "The mail servers are {provider}'s own, so there is nothing to edit here.",
  "settings.account.disconnect.title": "Disconnect {name}?",
  "settings.account.disconnect.body": "Mailroom stops syncing and sending for this account and gives up its access. The mail already here stays, and you can reconnect later.",
  "settings.account.disconnect.confirm": "Disconnect",
  "settings.account.disconnect.failed": "Could not disconnect: {error}",
  "settings.account.disconnect.revoked": "Disconnected. {provider} no longer lists Mailroom's access.",
  "settings.account.disconnect.manual.google": "Disconnected. Mailroom could not remove its access at Google itself: remove it in your Google Account, under Security, in the list of third-party apps and services with access.",
  "settings.account.disconnect.manual.microsoft": "Disconnected. Microsoft gives Mailroom no way to remove its own access: remove it in your Microsoft account, in the list of apps and services that can access your data.",
  "settings.account.disconnect.manual.unknown": "Disconnected. Remove Mailroom's access in your provider's account settings too.",
  "settings.operator.title": "Server sign-in problems",
  "settings.operator.notice": "{provider} is refusing this server's sign-in client ({code}) since {since}; {accounts} accounts affected. Check the client ID and secret in the server configuration: a client secret may have expired.",
  "pendingBanner.text.one": "1 action waiting for your approval",
  "pendingBanner.text.other": "{count} actions waiting for your approval",
  "pendingBanner.open": "Review",
  "settings.devices": "Signed-in devices",
  "devices.unavailable": "not available on this server",
  "devices.unavailableLong": "Signed-in devices are not available on this server.",
  "devices.asking": "Reading your signed-in devices…",
  "devices.state.count.one": "1 signed-in device",
  "devices.state.count.other": "{count} signed-in devices",
  "devices.state.none": "no sessions (development mode)",
  "devices.none": "This server signs in without sessions (development mode).",
  "devices.thisDevice": "This device",
  "devices.lastActive": "Last active {when}",
  "devices.signedIn": "Signed in {when}",
  "devices.from": "from {addr}",
  "devices.ago.now": "just now",
  "devices.ago.minutes": "{n} min ago",
  "devices.ago.hours": "{n} h ago",
  "devices.ago.days": "{n} d ago",
  "devices.signOut": "Sign out",
  "devices.signOutFor": "Sign out {name}",
  "devices.signOutHere": "Sign out of this device",
  "devices.signOutOthers": "Sign out all other devices",
  "devices.confirmOthers.one": "Sign out of 1 other device? It will need to sign in again.",
  "devices.confirmOthers.other": "Sign out of {count} other devices? They will need to sign in again.",
  "devices.done.one": "Signed out of {name}",
  "devices.done.count.one": "Signed out of 1 device",
  "devices.done.count.other": "Signed out of {count} devices",
  "devices.failed": "Could not sign out. Nothing was changed.",
  "devices.honest": "Signing out a device you don't recognise ends that sign-in, but whoever used it may still know your password, so change your password in your sign-in provider too. Ending your sessions there doesn't sign Mailroom devices out; signing them out here does.",
  "devices.pushNote": "Notifications set up before this release are not removed when a device signs out.",
  "apiKeys.unavailable": "not available on this server",
  "apiKeys.unavailableLong": "API keys are not available on this server.",
  "apiKeys.asking": "Reading your keys…",
  "apiKeys.never": "never",
  "apiKeys.neverUsed": "never used",
  "apiKeys.lastUsed": "last used {when}",
  "apiKeys.usage": "{used} of {budget} used in the last 24 hours",
  "apiKeys.preset.read": "Read only",
  "apiKeys.preset.read.explain": "search and read mail, labels and contacts",
  "apiKeys.preset.organise": "Organise",
  "apiKeys.preset.organise.explain": "Read only, plus moving, marking and labelling mail",
  "apiKeys.preset.assistant": "Assistant",
  "apiKeys.preset.assistant.explain": "Organise, plus writing drafts",
  "apiKeys.preset.custom": "Custom",
  "apiKeys.preset.custom.explain": "choose each permission, including sending and deleting",
  "apiKeys.scope.mail:read": "Read mail",
  "apiKeys.scope.search:read": "Search mail",
  "apiKeys.scope.labels:read": "See labels",
  "apiKeys.scope.contacts:read": "See contacts",
  "apiKeys.scope.mail:move": "Move, mark and label mail",
  "apiKeys.scope.labels:write": "Create and edit labels",
  "apiKeys.scope.mail:draft": "Write drafts",
  "apiKeys.scope.contacts:write": "Edit contacts",
  "apiKeys.scope.mail:send": "Send mail",
  "apiKeys.scope.mail:delete": "Delete mail (to Trash)",
  "apiKeys.summary.plus": "{preset} + {extra}",
  "apiKeys.restrict.all": "Every account",
  "apiKeys.restrict.only": "Only {accounts}",
  "apiKeys.restrict.unknown": "an account no longer here",
  "apiKeys.state.none": "no keys",
  "apiKeys.state.active": "Active",
  "apiKeys.state.activeCount.one": "1 active",
  "apiKeys.state.activeCount.other": "{count} active",
  "apiKeys.state.suspendedCount.one": "1 suspended",
  "apiKeys.state.suspendedCount.other": "{count} suspended",
  "apiKeys.state.waiting.one": "1 waiting for approval",
  "apiKeys.state.waiting.other": "{count} waiting for approval",
  "apiKeys.state.noReason": "no reason recorded",
  "apiKeys.state.suspendedShort": "Suspended: {reason}",
  "apiKeys.state.suspendedOn": "Suspended {when}: {reason}",
  "apiKeys.state.revokedShort": "Revoked {when}",
  "apiKeys.state.revokedOn": "Revoked {when}",
  "apiKeys.list.heading": "Your keys",
  "apiKeys.list.none": "You have no API keys. A key lets an AI tool or a script use your mail with only the permissions you give it.",
  "apiKeys.list.open": "Details",
  "apiKeys.list.openFor": "Details for {name}",
  "apiKeys.list.revoked.one": "1 revoked key",
  "apiKeys.list.revoked.other": "{count} revoked keys",
  "apiKeys.create.open": "Create a key…",
  "apiKeys.create.title": "Create an API key",
  "apiKeys.create.name": "Name",
  "apiKeys.create.nameHint": "Something that tells you which tool uses it, like \"Desktop assistant\".",
  "apiKeys.create.access": "What it can do",
  "apiKeys.create.scopes": "Permissions",
  "apiKeys.create.matchesPreset": "These are exactly the {preset} permissions.",
  "apiKeys.create.askFirst": "Ask me first",
  "apiKeys.create.askFirstFor": "Ask me before it can: {scope}",
  "apiKeys.create.askFirstHint": "Each request waits for you to approve or deny it here. Unanswered requests are denied after 24 hours.",
  "apiKeys.create.bothWarning": "This key can both send and delete mail. An agent with both can mail anyone as you and then remove the trace.",
  "apiKeys.create.bothLabel": "To allow both, type: {phrase}",
  "apiKeys.create.accounts": "Which accounts",
  "apiKeys.create.onlyThese": "Only these accounts",
  "apiKeys.create.restrictHint": "A key limited to some accounts sees only their mail and folders, and cannot use contacts.",
  "apiKeys.create.limits": "Limits",
  "apiKeys.create.rate": "Calls per minute",
  "apiKeys.create.rateHint": "1 to {max}. Leave empty for the default, {def}.",
  "apiKeys.create.budget": "Calls per 24 hours",
  "apiKeys.create.budgetHint": "1 to {max}. Leave empty for the default, {def}.",
  "apiKeys.create.suspend": "Suspend the key automatically if it misbehaves",
  "apiKeys.create.suspendHint": "For example, trying to delete more than {n} messages in {minutes} minutes. You can reactivate it here.",
  "apiKeys.create.problem.name": "Give the key a name (up to 100 characters).",
  "apiKeys.create.problem.scopes": "Choose at least one permission.",
  "apiKeys.create.problem.accounts": "Choose at least one account, or allow every account.",
  "apiKeys.create.problem.rate": "Calls per minute must be a whole number in the allowed range.",
  "apiKeys.create.problem.budget": "Calls per 24 hours must be a whole number in the allowed range.",
  "apiKeys.create.problem.sendAndDelete": "Type the phrase to allow both sending and deleting.",
  "apiKeys.create.ready": "Ready to create.",
  "apiKeys.create.submit": "Create key",
  "apiKeys.create.busy": "Creating…",
  "apiKeys.create.done": "Created \"{name}\".",
  "apiKeys.token.title": "Your new key: {name}",
  "apiKeys.token.once": "Copy this key now. You won't see it again: Mailroom keeps only a fingerprint of it.",
  "apiKeys.token.label": "API key",
  "apiKeys.token.copy": "Copy key",
  "apiKeys.token.done": "Done, I've saved the key",
  "apiKeys.copy.done": "Copied.",
  "apiKeys.copy.failed": "Couldn't copy. Select the text and copy it yourself.",
  "apiKeys.error.transport": "Mailroom couldn't be reached. Nothing was changed; try again.",
  "apiKeys.error.nameTaken": "You already have a key with this name.",
  "apiKeys.error.generic": "The server refused this ({type}).",
  "apiKeys.detail.back": "← All keys",
  "apiKeys.detail.missing": "This key isn't in your list any more.",
  "apiKeys.detail.state": "State",
  "apiKeys.detail.hint": "Key starts with",
  "apiKeys.detail.access": "What it can do",
  "apiKeys.detail.accounts": "Accounts",
  "apiKeys.detail.created": "Created",
  "apiKeys.detail.lastUsed": "Last used",
  "apiKeys.detail.usage": "Usage",
  "apiKeys.detail.edit": "Settings",
  "apiKeys.detail.save": "Save",
  "apiKeys.detail.saved": "Saved.",
  "apiKeys.detail.nothingChanged": "Nothing has changed.",
  "apiKeys.detail.suspend": "Suspend…",
  "apiKeys.detail.reactivate": "Reactivate…",
  "apiKeys.detail.revoke": "Revoke…",
  "apiKeys.suspend.title": "Suspend \"{name}\"?",
  "apiKeys.suspend.what": "Every call with this key will be refused until you reactivate it. Nothing else changes.",
  "apiKeys.suspend.submit": "Suspend",
  "apiKeys.suspend.busy": "Suspending…",
  "apiKeys.suspend.done": "Suspended.",
  "apiKeys.reactivate.title": "Reactivate \"{name}\"?",
  "apiKeys.reactivate.why": "It was suspended {when}: {reason}.",
  "apiKeys.reactivate.lastAnomaly": "The most recent unusual activity, {when}: {what}",
  "apiKeys.reactivate.what": "Reactivating lets the key make calls again, within its permissions and limits.",
  "apiKeys.reactivate.submit": "Reactivate",
  "apiKeys.reactivate.busy": "Reactivating…",
  "apiKeys.reactivate.done": "Reactivated.",
  "apiKeys.revoke.title": "Revoke \"{name}\"?",
  "apiKeys.revoke.what": "Anything using this key stops working at once. Its activity stays in the record.",
  "apiKeys.revoke.permanent": "This can't be undone. To give a tool access again, create a new key.",
  "apiKeys.revoke.submit": "Revoke key",
  "apiKeys.revoke.busy": "Revoking…",
  "apiKeys.revoke.done": "Revoked.",
  "apiKeys.activity.heading": "Activity",
  "apiKeys.activity.asking": "Reading the activity…",
  "apiKeys.activity.unavailable": "The activity couldn't be read.",
  "apiKeys.activity.none": "No activity yet.",
  "apiKeys.activity.bound": "Showing the latest {n} calls.",
  "apiKeys.outcome.ok": "Allowed",
  "apiKeys.outcome.okButFailed": "then failed ({error})",
  "apiKeys.outcome.pending": "Held for your approval",
  "apiKeys.outcome.approved": "Approved by you and run",
  "apiKeys.outcome.deniedByUser": "Denied by you",
  "apiKeys.outcome.expired": "Expired unanswered",
  "apiKeys.outcome.refused": "Refused",
  "apiKeys.outcome.anomaly": "Unusual activity",
  "apiKeys.outcome.other": "Recorded as \"{outcome}\"",
  "apiKeys.reason.method": "this action isn't available to API keys",
  "apiKeys.reason.sessionOnly": "only you can do this, never a key",
  "apiKeys.reason.restriction": "outside the accounts this key may use",
  "apiKeys.reason.arguments": "the request wasn't valid",
  "apiKeys.reason.scope": "the key doesn't have permission",
  "apiKeys.reason.scopeNeeds": "the key doesn't have permission ({scopes})",
  "apiKeys.reason.rate": "too many calls in a minute",
  "apiKeys.reason.budget": "the 24-hour budget is used up",
  "apiKeys.reason.approval": "an approval that was already used or withdrawn",
  "apiKeys.reason.massDelete": "it would have deleted more than {n} messages in {minutes} minutes",
  "apiKeys.reason.massSend": "it would have sent more than {n} messages in {minutes} minutes",
  "apiKeys.reason.pendingCap": "too many requests were already waiting for your approval",
  "apiKeys.reason.neverRan": "approved, but the server stopped before it ran; nothing was done",
  "apiKeys.reason.keyNotActive": "The key that asked was revoked or suspended since; nothing was done.",
  "apiKeys.reason.draftChanged": "The draft changed after the request; nothing was sent.",
  "apiKeys.reason.other": "refused ({reason})",
  "apiKeys.anomaly.massDelete": "tried to delete more than {n} messages in {minutes} minutes.",
  "apiKeys.anomaly.massSend": "tried to send more than {n} messages in {minutes} minutes.",
  "apiKeys.anomaly.offHours": "made changes during your quiet hours or overnight.",
  "apiKeys.anomaly.criticalRefusals": "kept trying things it isn't allowed to do.",
  "apiKeys.anomaly.suspended": "The key was suspended.",
  "apiKeys.anomaly.other": "rule \"{rule}\"",
  "apiKeys.method.unknown": "an unknown method",
  "apiKeys.suppressed.one": "+1 more refused call ({method}: {reason})",
  "apiKeys.suppressed.other": "+{count} more refused calls ({method}: {reason})",
  "apiKeys.untrusted.stripped": "(invisible direction characters removed)",
  "apiKeys.pending.heading": "Waiting for your approval",
  "apiKeys.pending.asking": "Checking for requests…",
  "apiKeys.pending.none": "Nothing is waiting for your approval.",
  "apiKeys.pending.kind.send": "Send a message",
  "apiKeys.pending.kind.delete": "Move messages to Trash",
  "apiKeys.pending.kind.other": "An action",
  "apiKeys.pending.fromKey": "asked by the key",
  "apiKeys.pending.to": "To",
  "apiKeys.pending.cc": "Cc",
  "apiKeys.pending.bcc": "Bcc",
  "apiKeys.pending.envelopeOnly": "Also delivered to (not shown in the message)",
  "apiKeys.pending.recipients": "Recipients",
  "apiKeys.pending.noRecipients": "none listed",
  "apiKeys.pending.subject": "Subject",
  "apiKeys.pending.noSubject": "(no subject)",
  "apiKeys.pending.writtenByAgent": "(written by the agent)",
  "apiKeys.pending.writtenBySenders": "(subjects written by the senders)",
  "apiKeys.pending.attachmentsLabel": "Attachments",
  "apiKeys.pending.noAttachments": "none",
  "apiKeys.pending.attachments.one": "1 file, {size}",
  "apiKeys.pending.attachments.other": "{count} files, {size} in total",
  "apiKeys.pending.alsoArchives": "Also archives",
  "apiKeys.pending.archiveCount.one": "1 message",
  "apiKeys.pending.archiveCount.other": "{count} messages",
  "apiKeys.pending.when": "When",
  "apiKeys.pending.sendNow": "When you approve, after your undo-send delay",
  "apiKeys.pending.sendAt": "Scheduled for {when}",
  "apiKeys.pending.moves": "Moves to Trash",
  "apiKeys.pending.deleteCount.one": "1 message",
  "apiKeys.pending.deleteCount.other": "{count} messages",
  "apiKeys.pending.subjectsAsking": "Reading their subjects…",
  "apiKeys.pending.subjectsUnavailable": "Their subjects couldn't be shown.",
  "apiKeys.pending.subjectsGone.one": "1 more wasn't found in that account; it may already have been moved or deleted.",
  "apiKeys.pending.subjectsGone.other": "{count} more weren't found in that account; they may already have been moved or deleted.",
  "apiKeys.pending.subjectsBound": "Subjects shown for the first {shown} of {total}.",
  "apiKeys.pending.namesBound": "The server kept {shown} of {total}.",
  "apiKeys.pending.restorable": "Can it be undone?",
  "apiKeys.pending.restorableText": "Yes: messages stay restorable from Trash for 30 days.",
  "apiKeys.pending.showAll": "Show all {count}",
  "apiKeys.pending.showFewer": "Show fewer",
  "apiKeys.pending.expiresIn": "Denied automatically in {left} if you don't answer.",
  "apiKeys.pending.expiredNow": "This request has expired.",
  "apiKeys.pending.approve": "Approve…",
  "apiKeys.pending.deny": "Deny",
  "apiKeys.pending.deciding": "Sending your answer…",
  "apiKeys.pending.state.pending": "Waiting",
  "apiKeys.pending.state.approved": "Approved",
  "apiKeys.pending.state.denied": "Denied",
  "apiKeys.pending.state.expired": "Expired",
  "apiKeys.pending.state.failed": "Failed",
  "apiKeys.approve.titleSend": "Approve this send?",
  "apiKeys.approve.titleDelete": "Approve moving these messages to Trash?",
  "apiKeys.approve.send.one": "The message will be sent to 1 recipient.",
  "apiKeys.approve.send.other": "The message will be sent to {count} recipients.",
  "apiKeys.approve.sendAt.one": "The message will be sent to 1 recipient at {when}.",
  "apiKeys.approve.sendAt.other": "The message will be sent to {count} recipients at {when}.",
  "apiKeys.approve.archive.one": "1 message will be archived.",
  "apiKeys.approve.archive.other": "{count} messages will be archived.",
  "apiKeys.approve.delete.one": "1 message will be moved to Trash.",
  "apiKeys.approve.delete.other": "{count} messages will be moved to Trash.",
  "apiKeys.approve.other": "The request will be run.",
  "apiKeys.approve.sendNote": "Mailroom checks the request again before running it: if the draft or the key changed, nothing is sent.",
  "apiKeys.approve.deleteNote": "Mailroom checks the request again before running it. Messages in Trash can be restored for 30 days.",
  "apiKeys.approve.submitSend": "Approve and send",
  "apiKeys.approve.submitDelete": "Approve and move to Trash",
  "apiKeys.decided.denied": "Denied. Nothing was done.",
  "apiKeys.decided.sent": "Approved. The message is on its way.",
  "apiKeys.decided.deleted.one": "Approved. 1 message was moved to Trash.",
  "apiKeys.decided.deleted.other": "Approved. {count} messages were moved to Trash.",
  "apiKeys.decided.failed": "Approved, but the server refused to run it: {why}",
  "apiKeys.decided.other": "The request is now {state}.",
  "apiKeys.decided.expired": "This request expired before you answered; nothing was done.",
  "apiKeys.decided.already": "This request was already decided.",
  "apiKeys.decided.notFound": "This request no longer exists.",
  "apiKeys.setup.heading": "Connect an AI tool",
  "apiKeys.setup.intro": "Mailroom serves its mail tools over the Model Context Protocol (MCP), so any MCP-capable client, with any model, can use them with an API key.",
  "apiKeys.setup.url": "URL",
  "apiKeys.setup.transport": "Transport",
  "apiKeys.setup.transportValue": "Streamable HTTP (sometimes called \"HTTP\" or \"streamable-http\")",
  "apiKeys.setup.header": "Header",
  "apiKeys.setup.placeholder": "mr_your_key_here",
  "apiKeys.setup.anyClient": "Most MCP clients that support remote servers accept a URL and a set of HTTP headers: any client that supports Streamable HTTP with a header works.",
  "apiKeys.setup.keepSecret": "Keep the key out of shared config files where your client allows an environment variable or a secrets store instead.",
  "apiKeys.setup.bridgeHeading": "Clients that only run local (stdio) servers",
  "apiKeys.setup.bridgeIntro": "A client that can only start a local process can reach Mailroom through a generic stdio-to-HTTP bridge, such as mcp-remote (an npm package, run with npx):",
  "apiKeys.setup.bridgeLabel": "Bridge configuration (JSON)",
  "apiKeys.setup.bridgeSpace": "There is no space after \"Authorization:\" on purpose: some clients do not escape spaces inside args, so the space lives in the environment variable instead.",
  "apiKeys.setup.plainHttp": "This Mailroom is served over plain HTTP, so the key travels unencrypted and the bridge config below includes --allow-http. Use it only on a trusted private network; otherwise serve Mailroom over HTTPS.",
  "apiKeys.setup.bridgeProcessList": "Values in args are visible in the process list to other users of the machine. To keep the key out of it, put the header in a file and pass --header-file /path/to/headers.txt instead of --header.",
  "apiKeys.setup.copyConfig": "Copy",
  "apiKeys.setup.examplesHeading": "Worked examples",
  "apiKeys.setup.examplesNote": "These two are examples, not requirements: any MCP client works the same way.",
  "apiKeys.setup.desktopExample": "A desktop chat client that runs local servers from a JSON config (for example, Claude Desktop): use the bridge block above as the entry under mcpServers, restart the client, and the Mailroom tools appear in its tool list. Clients whose built-in connectors expect OAuth cannot use a key directly; use the bridge.",
  "apiKeys.setup.localExample": "A local LLM tool with native remote-MCP support (for example, Hermes): configure a remote server with the URL and the Authorization header. The exact keys differ from tool to tool; the shape is usually:",
  "apiKeys.setup.localLabel": "Remote server configuration (YAML)",
  "apiKeys.setup.localNote": "Check your tool's own documentation for its spelling of url and headers. If it only runs stdio servers, use the bridge.",
  "apiKeys.setup.toolsHeading": "What an agent can do",
  "apiKeys.setup.toolsThisKey": "With this key",
  "apiKeys.setup.toolsSend": "Adding Send mail",
  "apiKeys.setup.toolsDelete": "Adding Delete mail",
  "apiKeys.setup.toolsNote": "A tool appears only when the key's permissions allow it; a tool called anyway is refused by the server. Deletes always go to Trash.",
} as const;

export type MessageKey = keyof typeof en;

type Catalog = Record<string, string>;

// The active catalog. A locale switcher only has to swap this map (and the
// <html lang>) — no component changes.
const catalogs: Record<string, Catalog> = { en };
let active = "en";

export function setLocale(locale: string): void {
  if (catalogs[locale]) active = locale;
}

/**
 * `<base>.one` or `<base>.other`, chosen by `count`, with `count` already bound.
 *
 * DELIBERATELY NOT A PLURAL-RULE ENGINE. English needs two forms and this app has
 * one locale; a real CDLR plural set (zero/one/two/few/many/other) is part of the
 * ICU decision deferred to v1, and the key shape here (`.one` / `.other`) is the
 * one every such library consumes, so adopting one later is adding forms rather
 * than rewriting call sites.
 *
 * It exists because the alternative — `count === 1 ? t("x.one") : t("x.other", …)`
 * — was being written out at every call site, and each copy is a chance to bind
 * `count` to only one of the two strings.
 */
export function tCount(
  base: string,
  count: number,
  params: Record<string, string | number> = {},
): string {
  return t(`${base}.${count === 1 ? "one" : "other"}` as MessageKey, { count, ...params });
}

export function t(key: MessageKey, params?: Record<string, string | number>): string {
  const catalog = catalogs[active] ?? en;
  const raw = catalog[key] ?? (en[key] as string) ?? key;
  if (!params) return raw;
  return raw.replace(/\{(\w+)\}/g, (m, name: string) =>
    name in params ? String(params[name]) : m,
  );
}
