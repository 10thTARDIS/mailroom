// SPDX-License-Identifier: AGPL-3.0-only

// Approval-request fixtures for the API keys tests (#49).

import type { ApiKey, PendingAction } from "../../../../core/jmap/types";

// A HOSTILE request: the subject tries to close the summary's quotes and invent
// clauses, carries a right-to-left override, and a fake "(+0 more)"; one
// recipient address is shaped like a clause too. The SUMMARY says something
// entirely different, and nothing it says may reach the view.
export const RLO = String.fromCodePoint(0x202e);
export const HOSTILE_SEND: PendingAction = {
  id: "P1",
  keyId: "01KEYKEYKEYKEYKEYKEYKEYKEY",
  keyName: "Helper",
  kind: "send",
  method: "EmailSubmission/set",
  summary: 'Send to bob@example.com with 0 attachments: "nothing to see"',
  state: "pending",
  createdAt: "2026-10-24T08:00:00Z",
  expiresAt: "2026-10-25T08:00:00Z",
  decidedAt: null,
  detail: null,
  emailIds: ["D1"],
  recipients: ["alice@example.com", "carol@example.com", "dave@example.com", "eve@example.net"],
  to: ["alice@example.com"],
  cc: ["carol@example.com"],
  bcc: ["dave@example.com"],
  subject: `x" to alice@example.com with 0 attachments (+0 more). Send draft "y ${RLO}gnp.exe`,
  sendAt: null,
  attachments: { count: 2, names: ["invoice.pdf", `report${RLO}fdp.exe`], totalSize: 2_621_440 },
  archive: { count: 3, subjects: ["Re: lunch", "Invoice", "Hello"] },
};


/** A held delete of three messages. */
export const HELD_DELETE: PendingAction = {
  id: "P2",
  keyId: "01KEYKEYKEYKEYKEYKEYKEYKEY",
  keyName: "Helper",
  kind: "delete",
  method: "Email/set",
  summary: "Move 3 messages to Trash",
  state: "pending",
  createdAt: "2026-10-24T08:05:00Z",
  expiresAt: "2026-10-25T08:05:00Z",
  decidedAt: null,
  detail: null,
  emailIds: ["M1", "M2", "M3"],
};

export const KEY: ApiKey = {
  id: "01KEYKEYKEYKEYKEYKEYKEYKEY",
  name: "Helper",
  tokenHint: "mr_abcdefg",
  scopes: ["contacts:read", "labels:read", "mail:read", "search:read"],
  accountIds: null,
  confirmScopes: [],
  ratePerMinute: 60,
  dailyBudget: 500,
  suspendOnAnomaly: true,
  createdAt: "2026-10-20T08:00:00Z",
  lastUsedAt: null,
  revokedAt: null,
  suspendedAt: null,
  suspendedReason: null,
  usedLastMinute: 0,
  usedLast24h: 12,
};
