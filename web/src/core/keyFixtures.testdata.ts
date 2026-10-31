// SPDX-License-Identifier: AGPL-3.0-only

// REAL RESPONSES, NOT HAND-MADE SHAPES (#796, #795). Every object below was
// captured from the B1 server (v1.93 key-management surface) running in the
// accessibility lane's stack: a dev-auth instance with a real crypto process
// and MAILROOM_WKD_DOMAIN=example.com. The correspondents' keys were made by
// gpg (alice: 10 days; carol: 150 seconds, read again after it lapsed); the
// keyExpired signature is the JSON internal/api's judgeSignature produced for
// a real go-crypto key that expired after signing. Ids, dates and fingerprints
// are as the server returned them; the only edit is the export's armoredKey,
// cut short. Tests that need "now" use CAPTURED_AT, the moment of the capture.
//
// Imported only by tests (the `.testdata` suffix keeps it out of the bundle:
// nothing in the app imports it).

import type { HeldContactKey, Identity, PgpSignature, RecipientKeyInfo, UserKey } from "./jmap/types";

/** When the capture ran: the keys' expiringSoon flags are true at this time. */
export const CAPTURED_AT = new Date("2026-10-21T09:53:00Z");

/** session.capabilities["urn:mailroom:pgp"] */
export const PGP_CAPABILITY = { encrypt: true, keyExpiryWarningDays: 30, keyManagement: true, signing: true };

export const IDENTITY_READER = {
  accountId: "01M5EXWH1PE7SD28A6XR1PCACJ",
  bcc: null,
  email: "reader@example.com",
  enabled: true,
  htmlSignature: "",
  id: "01M5EXWH47HY43GMMY3G2V37GT",
  isWildcard: false,
  mayDelete: true,
  name: "",
  replyTo: null,
  // As captured, BEFORE any key existed. A test that needs the bound key's
  // projection sets signingKeyId itself and says so.
  signingKeyId: null,
  textSignature: "",
} as unknown as Identity;

/** UserKey/generate {bind: true}, default expiry (two years): `.key`. */
export const USER_KEY_BOUND: UserKey = {
  canSign: true,
  createdAt: "2026-10-21T09:50:21Z",
  decryptOnly: false,
  expired: false,
  expiresAt: "2028-10-20T09:50:21Z",
  expiringSoon: false,
  fingerprint: "5DFC3469CCE54FD959E8B15AC64E290ED3B21461",
  id: "01M5EXWWF721X35BHHBN93MQKN",
  identityId: "01M5EXWH47HY43GMMY3G2V37GT",
  kind: "pgp",
  origin: "generated",
  revokedAt: null,
  usable: true,
  wkdPublished: false,
  wkdServable: true,
  wkdUrl: "https://example.com/.well-known/openpgpkey/hu/r14i94y6u37tn6cp8j8rc767jd9hofk3",
};

/** The same key after UserKey/set {wkdPublished: true} (UserKey/get). */
export const USER_KEY_BOUND_PUBLISHED: UserKey = { ...USER_KEY_BOUND, wkdPublished: true };

/** UserKey/generate {expiresDays: 10}, unbound: `.key`. Inside the warning window. */
export const USER_KEY_TEN_DAYS: UserKey = {
  canSign: true,
  createdAt: "2026-10-21T09:50:21Z",
  decryptOnly: false,
  expired: false,
  expiresAt: "2026-10-31T09:50:21Z",
  expiringSoon: true,
  fingerprint: "7330F062BF6AC6CEC2C74B6BC48DCCD61A905815",
  id: "01M5EXWWHCGES6KJ87RFYX3ZPX",
  identityId: null,
  kind: "pgp",
  origin: "generated",
  revokedAt: null,
  usable: true,
  wkdPublished: false,
  wkdServable: false,
  wkdUrl: null,
};

/** That key after UserKey/revoke (revoke also sets decryptOnly). */
export const USER_KEY_REVOKED: UserKey = {
  canSign: false,
  createdAt: "2026-10-21T09:50:21Z",
  decryptOnly: true,
  expired: false,
  expiresAt: "2027-10-21T09:50:22Z",
  expiringSoon: false,
  fingerprint: "7330F062BF6AC6CEC2C74B6BC48DCCD61A905815",
  id: "01M5EXWWHCGES6KJ87RFYX3ZPX",
  identityId: null,
  kind: "pgp",
  origin: "generated",
  revokedAt: "2026-10-21T09:50:22Z",
  usable: false,
  wkdPublished: false,
  wkdServable: false,
  wkdUrl: null,
};

/** UserKey/get's top-level WKD facts. */
export const USER_KEYS_WKD = { wkdConfigured: true, wkdDomain: "example.com" };

/** UserKey/extend {days: 365} on the ten-day key. */
export const EXTEND_RESPONSE = {
  accountId: "01M5EXWH064QFSG5AP2YQXERW0",
  expiresAt: "2027-10-21T09:50:22Z",
  id: "01M5EXWWHCGES6KJ87RFYX3ZPX",
};

/** UserKey/export {passphrase} (armoredKey cut short here). */
export const EXPORT_RESPONSE = {
  accountId: "01M5EXWH064QFSG5AP2YQXERW0",
  armoredKey: "-----BEGIN PGP PRIVATE KEY BLOCK-----\n\nxYYEatiK3RYJKwYBBAHaR...(truncated)",
  fileName: "mailroom-key-1a905815.asc",
  fingerprint: "7330F062BF6AC6CEC2C74B6BC48DCCD61A905815",
  id: "01M5EXWWHCGES6KJ87RFYX3ZPX",
};

/** UserKey/set refusals, as `notUpdated[id]`. */
export const SET_ERRORS = {
  notPublishable: {
    type: "notPublishable",
    description: "bind the key to an address first: WKD publishes a key AT an address",
    properties: ["wkdPublished"],
  },
  keyIsDefault: {
    type: "keyIsDefault",
    description:
      "that key is an identity's signing key; send identityId: null with decryptOnly: true to unbind it in the same step, or bind another key first",
    properties: ["decryptOnly"],
  },
  keyIsPublished: {
    type: "keyIsPublished",
    description: "that key is published in WKD at its address; unpublish it first",
    properties: ["identityId"],
  },
  oneChangeAtATime: {
    type: "oneChangeAtATime",
    description:
      "change one of identityId, wkdPublished or decryptOnly per update (decryptOnly: true may come with identityId: null)",
  },
};

/** Method errors, as the server sent them (`["error", {...}]`). */
export const METHOD_ERRORS = {
  extendTooLong: { type: "invalidArguments", description: "days must be between 1 and 3650" },
  exportShort: {
    type: "invalidArguments",
    description: "the passphrase must be at least 12 characters: it is the only thing protecting the file",
  },
  revokeWrongConfirm: {
    type: "invalidArguments",
    description: "confirmation does not match: key 01M5EXWWHCGES6KJ87RFYX3ZPX has fingerprint ending 1A905815",
  },
  importPublic: {
    type: "publicKeyRefused",
    description:
      "that is a PUBLIC key; your own key needs its secret half (a correspondent's key goes in ContactKey/import)",
  },
  importGarbage: { type: "invalidKey", description: "not a readable OpenPGP key" },
};

/** ContactKey/get: alice (10 days, inside the window). */
export const CONTACT_KEY_ALICE: HeldContactKey = {
  addresses: ["alice@example.org"],
  authenticated: false,
  contactId: "01M5EXWX1WJCT83WBVNDXBWW19",
  createdAt: "2026-10-21T09:50:18Z",
  expired: false,
  expiresAt: "2026-10-31T09:50:18Z",
  expiringSoon: true,
  fetchedAt: "2026-10-21T09:50:22Z",
  fingerprint: "15E4509E093BC2FAECE2D553302A36D1472E6F3E",
  hasEncryptionKey: true,
  id: "01M5EXWX20ECRQY7PBDYTF12EW",
  kind: "pgp",
  pinned: true,
  pinnedFor: ["alice@example.org"],
  refused: false,
  revoked: false,
  source: "manual",
  usable: true,
};

/** ContactKey/get: carol, read after her 150-second key lapsed. */
export const CONTACT_KEY_CAROL_EXPIRED: HeldContactKey = {
  addresses: ["carol@example.org"],
  authenticated: false,
  contactId: "01M5EXWX2F69AH2JJFQYJ0XYTX",
  createdAt: "2026-10-21T09:50:18Z",
  expired: true,
  expiresAt: "2026-10-21T09:52:48Z",
  expiringSoon: false,
  fetchedAt: "2026-10-21T09:50:22Z",
  fingerprint: "CECA54264C0F29DD140262DE2E0933C0CF5D376F",
  hasEncryptionKey: true,
  id: "01M5EXWX2J055WBS67RG4DZM70",
  kind: "pgp",
  pinned: true,
  pinnedFor: ["carol@example.org"],
  refused: false,
  revoked: false,
  source: "manual",
  usable: false,
};

/** The back-referenced Contact/get {properties: [displayName, addresses]}. */
export const CONTACTS_FOR_KEYS = [
  { addresses: [{ address: "carol@example.org", reachedMeAt: null }], displayName: "", id: "01M5EXWX2F69AH2JJFQYJ0XYTX" },
  { addresses: [{ address: "alice@example.org", reachedMeAt: null }], displayName: "", id: "01M5EXWX1WJCT83WBVNDXBWW19" },
];

/** ContactKey/set {destroy: [carol, an id nobody holds]}. */
export const CONTACT_KEY_DESTROY = {
  destroyed: ["01M5EXWX2J055WBS67RG4DZM70"],
  notDestroyed: { "01ARZ3NDEKTSV4RRFFQ69G5FAV": { type: "notFound", description: "no such key" } },
};

/** RecipientKey/lookup while both keys were live. */
export const LOOKUP_ALICE_SOON: RecipientKeyInfo = {
  address: "alice@example.org",
  expiredAt: null,
  expiresAt: "2026-10-31T09:50:18Z",
  expiringSoon: true,
  fingerprint: "15E4509E093BC2FAECE2D553302A36D1472E6F3E",
  keyChanged: false,
  pinnedFingerprint: null,
  reason: null,
  source: "manual",
  usable: true,
  wkd: "skipped",
};

/** RecipientKey/lookup after carol's key lapsed. */
export const LOOKUP_CAROL_EXPIRED: RecipientKeyInfo = {
  address: "carol@example.org",
  expiredAt: "2026-10-21T09:52:48Z",
  expiresAt: null,
  expiringSoon: false,
  fingerprint: null,
  keyChanged: false,
  pinnedFingerprint: null,
  reason: "expired",
  source: null,
  usable: false,
  wkd: "none",
};

/** RecipientKey/lookup for an address nobody holds a key for. */
export const LOOKUP_NO_KEY: RecipientKeyInfo = {
  address: "nobody@example.net",
  expiredAt: null,
  expiresAt: null,
  expiringSoon: false,
  fingerprint: null,
  keyChanged: false,
  pinnedFingerprint: null,
  reason: "noKey",
  source: null,
  usable: false,
  wkd: "none",
};

/** Email.pgpSignature for a message signed by a held WKD key that expired after
 * it signed (internal/api judgeSignature, detached shape). */
export const SIGNATURE_KEY_EXPIRED: PgpSignature = {
  state: "bad",
  address: "alice@example.com",
  fingerprint: "9588BA6B270E4F9D157239CF0A895957BDAB2CAC",
  reason: "keyExpired",
  signedAt: "2026-09-16T09:49:12Z",
  keyExpiredAt: "2026-10-11T09:49:12Z",
  validWhenSigned: true,
};

/** The same expired signer, but a key that does not name From: a plain bad. */
export const SIGNATURE_PLAIN_BAD: PgpSignature = { state: "bad", address: "alice@example.com" };
