// SPDX-License-Identifier: AGPL-3.0-only

// Approval-request fixtures for the API keys tests (#49).

import type { ApiKey, OAuthClient, PendingAction } from "../../../../core/jmap/types";

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

/** A grant to an app that identifies itself by a metadata document (verified host). */
export const APP_VERIFIED: ApiKey = {
  id: "01APPAPPAPPAPPAPPAPPAPPAPP",
  name: "Example Assistant",
  tokenHint: null,
  kind: "oauth",
  oauthClient: {
    id: "https://app.example.com/client.json",
    name: "Example Assistant",
    kind: "cimd",
    redirectHost: "app.example.com",
  },
  families: [
    { id: "01FAMFAMFAMFAMFAMFAMFAMFA1", createdAt: "2026-10-28T09:00:00Z", lastUsedAt: "2026-11-02T10:00:00Z", expiresAt: "2026-12-02T10:00:00Z" },
    { id: "01FAMFAMFAMFAMFAMFAMFAMFA2", createdAt: "2026-10-30T09:00:00Z", lastUsedAt: "2026-10-30T09:30:00Z", expiresAt: "2026-11-29T09:30:00Z" },
  ],
  scopes: ["mail:read", "mail:send", "search:read"],
  accountIds: null,
  confirmScopes: ["mail:send"],
  ratePerMinute: 60,
  dailyBudget: 500,
  suspendOnAnomaly: true,
  createdAt: "2026-10-28T09:00:00Z",
  lastUsedAt: "2026-11-02T10:00:00Z",
  revokedAt: null,
  suspendedAt: null,
  suspendedReason: null,
  usedLastMinute: 0,
  usedLast24h: 7,
};

/** A grant to an app registered by Dynamic Client Registration: its name is self-asserted. */
export const APP_UNVERIFIED: ApiKey = {
  ...APP_VERIFIED,
  id: "01APPUNVERIFIEDUNVERIFIED1",
  name: "Mystery Tool",
  oauthClient: { id: "mc_0123456789", name: "Mystery Tool", kind: "registered", redirectHost: "tool.example.com" },
  families: [],
  scopes: ["mail:read"],
  confirmScopes: [],
  lastUsedAt: null,
};

/** A grant to an app the user registered in Settings. */
export const APP_MANUAL: ApiKey = {
  ...APP_VERIFIED,
  id: "01APPMANUALMANUALMANUAL01",
  name: "My Script",
  oauthClient: { id: "mc_manual00001", name: "My Script", kind: "manual", redirectHost: "127.0.0.1" },
  families: [],
  scopes: ["mail:read", "mail:move"],
  confirmScopes: [],
  suspendedAt: "2026-11-01T02:00:00Z",
  suspendedReason: "mass delete: more than 50 deletes in 10 minutes",
};

export const CLIENT: OAuthClient = {
  id: "mc_manual00001",
  name: "My Script",
  redirectUris: ["http://127.0.0.1/callback", "https://script.example.com/cb"],
  createdAt: "2026-10-27T08:00:00Z",
};

/** The app-related members of ApiKeysApi for a test that does not exercise them. */
export const NO_APPS = {
  clients: async () => [] as OAuthClient[],
  createClient: async () => ({ ok: false as const, error: { type: "unused" } }),
  deleteClient: async () => ({ ok: true as const }),
  endConnection: async () => ({ ok: true as const }),
};
