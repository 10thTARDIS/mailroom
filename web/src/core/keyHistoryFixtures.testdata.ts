// SPDX-License-Identifier: AGPL-3.0-only

// REAL RESPONSES FROM THE C1 SERVER (#815, #813, #812, #817, #818), not hand-made
// shapes. Every object marked CAPTURED was returned by the v1.93 C1 surface
// running in the accessibility lane's stack (a dev-auth instance with a real
// crypto process and MAILROOM_WKD_DOMAIN=example.com), driven over JMAP by
// scripts the C2 increment ran once. The correspondents' keys and the two
// imported own keys were made by gpg: alice has three user IDs (one revoked,
// one whose name holds markup), bob was refused and then removed, carol's key
// lasted 150 seconds and was read again after it lapsed.
//
// Ids, dates and fingerprints are as the server returned them. Objects marked
// DERIVED are states the stack could not produce, built from a captured object
// with the change named beside them (the stack has no reachable WKD server, so
// a renewal, an unchanged copy, a different key and an unreachable directory
// are derived from the captured `notFound` refresh; a retirement from before
// v1.93 is derived by clearing a captured `retiredAt`; a store failure after
// generation copies the server's error text from user_key_replace.go).
//
// Imported only by tests (the `.testdata` suffix keeps it out of the bundle).

import type {
  ContactKeyEvent,
  ContactKeyRefreshResult,
  HeldContactKey,
  Identity,
  RecipientKeyInfo,
  UserKey,
  UserKeyReplaceResult,
} from "./jmap/types";

/** When the capture ran (the first call). */
export const C1_CAPTURED_AT = new Date("2026-10-21T18:22:48.907Z");

/** CAPTURED: Identity/get, before any key existed. */
export const C1_IDENTITY = {
  "accountId": "01M5FV6HVSFC0G0ZQKRGGWEC8C",
  "bcc": null,
  "email": "reader@example.com",
  "enabled": true,
  "htmlSignature": "",
  "id": "01M5FV6J1C5EV7SY7V8SEVARTX",
  "isWildcard": false,
  "mayDelete": true,
  "name": "",
  "replyTo": null,
  "signingKeyId": null,
  "textSignature": ""
} as unknown as Identity;

/** CAPTURED: UserKey/generate {expiresAt: "2026-11-04", bind: true} — `.key`. The
 * date is the end of that day in UTC, and 14 days out it is inside the 30-day
 * warning window at once (expiringSoon). */
export const C1_KEY_ON_A_DATE: UserKey = {
  "boundAddressListed": true,
  "canSign": true,
  "createdAt": "2026-10-21T18:22:50Z",
  "decryptOnly": false,
  "expired": false,
  "expiresAt": "2026-11-04T23:59:59Z",
  "expiringSoon": true,
  "fingerprint": "C00AD4BD7C9DC7DBFE26F43B9D5865F27FE83786",
  "id": "01M5FV78665PW9MY7EG56FWXH8",
  "identityId": "01M5FV6J1C5EV7SY7V8SEVARTX",
  "kind": "pgp",
  "origin": "generated",
  "replacedBy": null,
  "retiredAt": null,
  "revokedAt": null,
  "usable": true,
  "userIds": [
    {
      "email": "reader@example.com",
      "name": "",
      "primary": true,
      "revoked": false
    }
  ],
  "wkdPublished": false,
  "wkdServable": true,
  "wkdUrl": "https://example.com/.well-known/openpgpkey/hu/r14i94y6u37tn6cp8j8rc767jd9hofk3"
};
export const C1_GENERATE_DATE_SENT = "2026-11-04";

/** CAPTURED: UserKey/extend {expiresAt: "2026-11-30"} on another key. */
export const C1_EXTEND_TO_A_DATE = {
  "accountId": "01M5FV6HRRTWDEAMPRAGPZM37Y",
  "expiresAt": "2026-11-30T23:59:59Z",
  "id": "01M5FV78AHQPRF9P11B29SGQ2K"
};
export const C1_EXTEND_DATE_SENT = "2026-11-30";

/** CAPTURED: method errors, as the server sent them (`["error", {...}]`). */
export const C1_METHOD_ERRORS = {
  "generateBoth": {
    "type": "invalidArguments",
    "description": "give expiresDays or expiresAt, not both"
  },
  "generatePast": {
    "type": "invalidArguments",
    "description": "expiresAt must be at least one day and at most 3650 days from now"
  },
  "generateTooFar": {
    "type": "invalidArguments",
    "description": "expiresAt must be at least one day and at most 3650 days from now"
  },
  "replaceRetired": {
    "type": "keyRetired",
    "description": "that key is already retired; generate a new key for the address instead"
  },
  "replaceUnbound": {
    "type": "keyNotBound",
    "description": "that key is not bound to an address, so there is nothing to put a new key in place of; generate one instead"
  },
  "replaceRevoked": {
    "type": "keyRevoked",
    "description": "that key is revoked; generate a new key for the address instead"
  },
  "replaceBoth": {
    "type": "invalidArguments",
    "description": "give expiresDays or expiresAt, not both"
  },
  "historyUnknown": {
    "type": "notFound",
    "description": "no such key"
  },
  "refreshAgain": {
    "type": "rateLimited",
    "description": "that key was checked at 2026-10-21T18:22:52Z; try again in 900s"
  },
  "refreshRemoved": {
    "type": "notFound",
    "description": "no such key"
  }
};

/** DERIVED: UserKey/replace whose store step failed after the new key was
 * made. The stack cannot inject that failure; the type and the text are
 * user_key_replace.go's, with the id of a key the capture made. */
export const C1_REPLACE_FAILED_AFTER_GENERATION = {
  type: "serverFail",
  description:
    "the replacement could not be completed. A new key (01M5FV79CY1DWKTJ2JKQD7YDTY) was made but not put in place; your current key is unchanged.",
};

/** CAPTURED: UserKey/set binding a key whose only user ID names another
 * address: the bind is made, and the server warns. */
export const C1_BIND_NOT_LISTED = {
  "01M5FV78P0K4NW0AH52MN1VJVA": {
    "boundAddressListed": false,
    "warning": "addressNotInKey"
  }
};
/** CAPTURED: binding a key that does name the address. */
export const C1_BIND_LISTED = {
  "01M5FV78KKEJE68VCCEA1T0ZQS": {
    "boundAddressListed": true
  }
};
/** CAPTURED: a publish (no bind): the update answers null. */
export const C1_PUBLISH_UPDATED = {
  "01M5FV78665PW9MY7EG56FWXH8": null
};

/** CAPTURED: UserKey/replace on the published, bound date key ({expiresAt} 400
 * days out): the new key bound and published, the old one retired with
 * retiredAt and replacedBy. */
export const C1_REPLACE: UserKeyReplaceResult = {
  "boundTo": "01M5FV6J1C5EV7SY7V8SEVARTX",
  "fingerprint": "FA75449067ABCE3788DB4AA18173F6FB6072568B",
  "id": "01M5FV79CY1DWKTJ2JKQD7YDTY",
  "key": {
    "boundAddressListed": true,
    "canSign": true,
    "createdAt": "2026-10-21T18:22:51Z",
    "decryptOnly": false,
    "expired": false,
    "expiresAt": "2027-11-25T23:59:59Z",
    "expiringSoon": false,
    "fingerprint": "FA75449067ABCE3788DB4AA18173F6FB6072568B",
    "id": "01M5FV79CY1DWKTJ2JKQD7YDTY",
    "identityId": "01M5FV6J1C5EV7SY7V8SEVARTX",
    "kind": "pgp",
    "origin": "generated",
    "replacedBy": null,
    "retiredAt": null,
    "revokedAt": null,
    "usable": true,
    "userIds": [
      {
        "email": "reader@example.com",
        "name": "",
        "primary": true,
        "revoked": false
      }
    ],
    "wkdPublished": true,
    "wkdServable": true,
    "wkdUrl": "https://example.com/.well-known/openpgpkey/hu/r14i94y6u37tn6cp8j8rc767jd9hofk3"
  },
  "oldKey": {
    "boundAddressListed": true,
    "canSign": false,
    "createdAt": "2026-10-21T18:22:50Z",
    "decryptOnly": true,
    "expired": false,
    "expiresAt": "2026-11-04T23:59:59Z",
    "expiringSoon": false,
    "fingerprint": "C00AD4BD7C9DC7DBFE26F43B9D5865F27FE83786",
    "id": "01M5FV78665PW9MY7EG56FWXH8",
    "identityId": "01M5FV6J1C5EV7SY7V8SEVARTX",
    "kind": "pgp",
    "origin": "generated",
    "replacedBy": "01M5FV79CY1DWKTJ2JKQD7YDTY",
    "retiredAt": "2026-10-21T18:22:51Z",
    "revokedAt": null,
    "usable": false,
    "userIds": [
      {
        "email": "reader@example.com",
        "name": "",
        "primary": true,
        "revoked": false
      }
    ],
    "wkdPublished": false,
    "wkdServable": true,
    "wkdUrl": "https://example.com/.well-known/openpgpkey/hu/r14i94y6u37tn6cp8j8rc767jd9hofk3"
  },
  "published": true,
  "replacedId": "01M5FV78665PW9MY7EG56FWXH8"
};

/** CAPTURED (UserKey/get after the replace): the key UserKey/replace made: bound, published. */
export const C1_KEY_REPLACEMENT: UserKey = {
  "boundAddressListed": true,
  "canSign": true,
  "createdAt": "2026-10-21T18:22:51Z",
  "decryptOnly": false,
  "expired": false,
  "expiresAt": "2027-11-25T23:59:59Z",
  "expiringSoon": false,
  "fingerprint": "FA75449067ABCE3788DB4AA18173F6FB6072568B",
  "id": "01M5FV79CY1DWKTJ2JKQD7YDTY",
  "identityId": "01M5FV6J1C5EV7SY7V8SEVARTX",
  "kind": "pgp",
  "origin": "generated",
  "replacedBy": null,
  "retiredAt": null,
  "revokedAt": null,
  "usable": true,
  "userIds": [
    {
      "email": "reader@example.com",
      "name": "",
      "primary": true,
      "revoked": false
    }
  ],
  "wkdPublished": true,
  "wkdServable": true,
  "wkdUrl": "https://example.com/.well-known/openpgpkey/hu/r14i94y6u37tn6cp8j8rc767jd9hofk3"
};

/** CAPTURED (UserKey/get after the replace): gpg-made, imported, its only user ID names someone@example.net, bound to reader@example.com (boundAddressListed false). */
export const C1_KEY_NOT_LISTED: UserKey = {
  "boundAddressListed": false,
  "canSign": true,
  "createdAt": "2026-10-21T18:22:50Z",
  "decryptOnly": false,
  "expired": false,
  "expiresAt": "2028-10-20T18:22:42Z",
  "expiringSoon": false,
  "fingerprint": "2A9178DAAF7F2824F1B5AFE93C8EACCC7C5795B6",
  "id": "01M5FV78P0K4NW0AH52MN1VJVA",
  "identityId": "01M5FV6J1C5EV7SY7V8SEVARTX",
  "kind": "pgp",
  "origin": "imported",
  "replacedBy": null,
  "retiredAt": null,
  "revokedAt": null,
  "usable": true,
  "userIds": [
    {
      "email": "someone@example.net",
      "name": "Mailroom Test",
      "primary": true,
      "revoked": false
    }
  ],
  "wkdPublished": false,
  "wkdServable": true,
  "wkdUrl": "https://example.com/.well-known/openpgpkey/hu/r14i94y6u37tn6cp8j8rc767jd9hofk3"
};

/** CAPTURED (UserKey/get after the replace): gpg-made, imported, two user IDs (the primary is the second one gpg added), bound. */
export const C1_KEY_TWO_USER_IDS: UserKey = {
  "boundAddressListed": true,
  "canSign": true,
  "createdAt": "2026-10-21T18:22:50Z",
  "decryptOnly": false,
  "expired": false,
  "expiresAt": "2028-10-20T18:22:41Z",
  "expiringSoon": false,
  "fingerprint": "FC1B1A5F15866340FBCEA210203B989E907282DF",
  "id": "01M5FV78KKEJE68VCCEA1T0ZQS",
  "identityId": "01M5FV6J1C5EV7SY7V8SEVARTX",
  "kind": "pgp",
  "origin": "imported",
  "replacedBy": null,
  "retiredAt": null,
  "revokedAt": null,
  "usable": true,
  "userIds": [
    {
      "email": "reader@work.example",
      "name": "Reader at work",
      "primary": true,
      "revoked": false
    },
    {
      "email": "reader@example.com",
      "name": "Reader Example",
      "primary": false,
      "revoked": false
    }
  ],
  "wkdPublished": false,
  "wkdServable": true,
  "wkdUrl": "https://example.com/.well-known/openpgpkey/hu/r14i94y6u37tn6cp8j8rc767jd9hofk3"
};

/** CAPTURED (UserKey/get after the replace): extended to a date, then revoked: retiredAt and revokedAt the same second. */
export const C1_KEY_REVOKED: UserKey = {
  "boundAddressListed": null,
  "canSign": false,
  "createdAt": "2026-10-21T18:22:50Z",
  "decryptOnly": true,
  "expired": false,
  "expiresAt": "2026-11-30T23:59:59Z",
  "expiringSoon": false,
  "fingerprint": "AFF00C65E1A698DC062E86B1D0830A0BCF9B8585",
  "id": "01M5FV78AHQPRF9P11B29SGQ2K",
  "identityId": null,
  "kind": "pgp",
  "origin": "generated",
  "replacedBy": null,
  "retiredAt": "2026-10-21T18:22:51Z",
  "revokedAt": "2026-10-21T18:22:51Z",
  "usable": false,
  "userIds": [
    {
      "email": "reader@example.com",
      "name": "",
      "primary": true,
      "revoked": false
    }
  ],
  "wkdPublished": false,
  "wkdServable": false,
  "wkdUrl": null
};

/** CAPTURED (UserKey/get after the replace): the date key after replace: retired, replacedBy the replacement. */
export const C1_KEY_REPLACED: UserKey = {
  "boundAddressListed": true,
  "canSign": false,
  "createdAt": "2026-10-21T18:22:50Z",
  "decryptOnly": true,
  "expired": false,
  "expiresAt": "2026-11-04T23:59:59Z",
  "expiringSoon": false,
  "fingerprint": "C00AD4BD7C9DC7DBFE26F43B9D5865F27FE83786",
  "id": "01M5FV78665PW9MY7EG56FWXH8",
  "identityId": "01M5FV6J1C5EV7SY7V8SEVARTX",
  "kind": "pgp",
  "origin": "generated",
  "replacedBy": "01M5FV79CY1DWKTJ2JKQD7YDTY",
  "retiredAt": "2026-10-21T18:22:51Z",
  "revokedAt": null,
  "usable": false,
  "userIds": [
    {
      "email": "reader@example.com",
      "name": "",
      "primary": true,
      "revoked": false
    }
  ],
  "wkdPublished": false,
  "wkdServable": true,
  "wkdUrl": "https://example.com/.well-known/openpgpkey/hu/r14i94y6u37tn6cp8j8rc767jd9hofk3"
};

/** CAPTURED: the whole UserKey/get list after the replace, in the server's order. */
export const C1_KEYS_AFTER_REPLACE: UserKey[] = [C1_KEY_REPLACEMENT, C1_KEY_NOT_LISTED, C1_KEY_TWO_USER_IDS, C1_KEY_REVOKED, C1_KEY_REPLACED];

/** DERIVED from C1_KEY_REPLACED: a key retired before v1.93, whose retirement
 * time was never recorded (retiredAt null, no replacedBy). */
export const C1_KEY_RETIRED_NOT_RECORDED: UserKey = { ...C1_KEY_REPLACED, retiredAt: null, replacedBy: null };

/** CAPTURED (ContactKey/get {includeRemoved: true}): 150 seconds, read while it was still live. */
export const C1_CONTACT_KEY_CAROL: HeldContactKey = {
  "addresses": [
    "carol@example.org"
  ],
  "authenticated": false,
  "contactId": "01M5FV79ZK6YE48J5PZH0P42WB",
  "createdAt": "2026-10-21T18:22:41Z",
  "expired": false,
  "expiresAt": "2026-10-21T18:25:11Z",
  "expiringSoon": true,
  "fetchedAt": "2026-10-21T18:22:51Z",
  "fingerprint": "BAB603A8AAE478F90E9ACAED23DC15553303AC5F",
  "hasEncryptionKey": true,
  "id": "01M5FV7A03Y8S9EB70K40ACJPW",
  "kind": "pgp",
  "pinned": true,
  "pinnedFor": [
    "carol@example.org"
  ],
  "refused": false,
  "removedAt": null,
  "revoked": false,
  "source": "manual",
  "usable": true,
  "userIds": [
    {
      "email": "carol@example.org",
      "name": "Carol Example",
      "primary": true,
      "revoked": false
    }
  ]
};

/** CAPTURED (ContactKey/get {includeRemoved: true}): refused, then removed (removedAt set; listed only with includeRemoved). */
export const C1_CONTACT_KEY_BOB_REMOVED: HeldContactKey = {
  "addresses": [
    "bob@example.org"
  ],
  "authenticated": false,
  "contactId": "01M5FV79XRAX4712HVH998BW00",
  "createdAt": "2026-10-21T18:22:41Z",
  "expired": false,
  "expiresAt": "2028-10-20T18:22:41Z",
  "expiringSoon": false,
  "fetchedAt": "2026-10-21T18:22:51Z",
  "fingerprint": "EFBE22BB0259C046C679B00F43203314553755D3",
  "hasEncryptionKey": true,
  "id": "01M5FV79Y3R37Y3ZFF3P0RXQYB",
  "kind": "pgp",
  "pinned": false,
  "pinnedFor": [],
  "refused": true,
  "removedAt": "2026-10-21T18:22:52Z",
  "revoked": false,
  "source": "manual",
  "usable": true,
  "userIds": [
    {
      "email": "bob@example.org",
      "name": "Bob Example",
      "primary": true,
      "revoked": false
    }
  ]
};

/** CAPTURED (ContactKey/get {includeRemoved: true}): three user IDs: "Alice <b>Bold</b> <alice@example.org>" (which the server parses as name "Alice", email "b", and lists as primary), alice@, and a revoked alice.old@. */
export const C1_CONTACT_KEY_ALICE: HeldContactKey = {
  "addresses": [
    "alice.old@example.org",
    "alice@example.org",
    "b"
  ],
  "authenticated": false,
  "contactId": "01M5FV79V2X5BXJ6CXM6PM12QM",
  "createdAt": "2026-10-21T18:22:40Z",
  "expired": false,
  "expiresAt": "2026-10-31T18:22:40Z",
  "expiringSoon": true,
  "fetchedAt": "2026-10-21T18:22:51Z",
  "fingerprint": "BF68BDC49B109EC9055EB6B0B1C0155859051703",
  "hasEncryptionKey": true,
  "id": "01M5FV79VSYGZY2DHS14ES4DV1",
  "kind": "pgp",
  "pinned": true,
  "pinnedFor": [
    "alice@example.org"
  ],
  "refused": false,
  "removedAt": null,
  "revoked": false,
  "source": "manual",
  "usable": true,
  "userIds": [
    {
      "email": "b",
      "name": "Alice",
      "primary": true,
      "revoked": false
    },
    {
      "email": "alice@example.org",
      "name": "Alice Example",
      "primary": false,
      "revoked": false
    },
    {
      "email": "alice.old@example.org",
      "name": "Alice Old",
      "primary": false,
      "revoked": true
    }
  ]
};

/** CAPTURED: carol's key read again after it lapsed (ContactKey/get). */
export const C1_CONTACT_KEY_CAROL_EXPIRED: HeldContactKey = {
  "addresses": [
    "carol@example.org"
  ],
  "authenticated": false,
  "contactId": "01M5FV79ZK6YE48J5PZH0P42WB",
  "createdAt": "2026-10-21T18:22:41Z",
  "expired": true,
  "expiresAt": "2026-10-21T18:25:11Z",
  "expiringSoon": false,
  "fetchedAt": "2026-10-21T18:22:51Z",
  "fingerprint": "BAB603A8AAE478F90E9ACAED23DC15553303AC5F",
  "hasEncryptionKey": true,
  "id": "01M5FV7A03Y8S9EB70K40ACJPW",
  "kind": "pgp",
  "pinned": true,
  "pinnedFor": [
    "carol@example.org"
  ],
  "refused": false,
  "removedAt": null,
  "revoked": false,
  "source": "manual",
  "usable": false,
  "userIds": [
    {
      "email": "carol@example.org",
      "name": "Carol Example",
      "primary": true,
      "revoked": false
    }
  ]
};

/** CAPTURED: the back-referenced Contact/get {properties: [displayName, addresses]}. */
export const C1_CONTACTS = [
  {
    "addresses": [
      {
        "address": "carol@example.org",
        "reachedMeAt": null
      }
    ],
    "displayName": "",
    "id": "01M5FV79ZK6YE48J5PZH0P42WB"
  },
  {
    "addresses": [
      {
        "address": "bob@example.org",
        "reachedMeAt": null
      }
    ],
    "displayName": "",
    "id": "01M5FV79XRAX4712HVH998BW00"
  },
  {
    "addresses": [
      {
        "address": "alice@example.org",
        "reachedMeAt": null
      }
    ],
    "displayName": "",
    "id": "01M5FV79V2X5BXJ6CXM6PM12QM"
  }
];

/** CAPTURED: RecipientKey/lookup for carol after her key lapsed. */
export const C1_LOOKUP_CAROL_EXPIRED: RecipientKeyInfo = {
  "address": "carol@example.org",
  "expiredAt": "2026-10-21T18:25:11Z",
  "expiresAt": null,
  "expiringSoon": false,
  "fingerprint": null,
  "keyChanged": false,
  "pinnedFingerprint": null,
  "reason": "expired",
  "source": null,
  "usable": false,
  "wkd": "none"
};

/** CAPTURED: ContactKey/history for bob (imported, refused, removed). */
export const C1_HISTORY_BOB: ContactKeyEvent[] = [
  {
    "at": "2026-10-21T18:22:51Z",
    "expiresAt": "2028-10-20T18:22:41Z",
    "kind": "first_seen",
    "source": "manual"
  },
  {
    "at": "2026-10-21T18:22:52Z",
    "expiresAt": "2028-10-20T18:22:41Z",
    "kind": "refused",
    "source": "user"
  },
  {
    "at": "2026-10-21T18:22:52Z",
    "expiresAt": "2028-10-20T18:22:41Z",
    "kind": "removed",
    "source": "user"
  }
];
/** CAPTURED: ContactKey/history for alice (imported only). */
export const C1_HISTORY_ALICE: ContactKeyEvent[] = [
  {
    "at": "2026-10-21T18:22:51Z",
    "expiresAt": "2026-10-31T18:22:40Z",
    "kind": "first_seen",
    "source": "manual"
  }
];

/** CAPTURED: ContactKey/refresh for alice: the stack's WKD answered "none". */
export const C1_REFRESH_ALICE_NOT_FOUND: ContactKeyRefreshResult = {
  "addresses": [
    {
      "address": "alice@example.org",
      "outcome": "none"
    }
  ],
  "checkedAt": "2026-10-21T18:22:52Z",
  "id": "01M5FV79VSYGZY2DHS14ES4DV1",
  "key": {
    "addresses": [
      "alice.old@example.org",
      "alice@example.org",
      "b"
    ],
    "authenticated": false,
    "contactId": "01M5FV79V2X5BXJ6CXM6PM12QM",
    "createdAt": "2026-10-21T18:22:40Z",
    "expired": false,
    "expiresAt": "2026-10-31T18:22:40Z",
    "expiringSoon": true,
    "fetchedAt": "2026-10-21T18:22:51Z",
    "fingerprint": "BF68BDC49B109EC9055EB6B0B1C0155859051703",
    "hasEncryptionKey": true,
    "id": "01M5FV79VSYGZY2DHS14ES4DV1",
    "kind": "pgp",
    "pinned": true,
    "pinnedFor": [
      "alice@example.org"
    ],
    "refused": false,
    "removedAt": null,
    "revoked": false,
    "source": "manual",
    "usable": true,
    "userIds": [
      {
        "email": "b",
        "name": "Alice",
        "primary": true,
        "revoked": false
      },
      {
        "email": "alice@example.org",
        "name": "Alice Example",
        "primary": false,
        "revoked": false
      },
      {
        "email": "alice.old@example.org",
        "name": "Alice Old",
        "primary": false,
        "revoked": true
      }
    ]
  },
  "result": "notFound"
};
/** CAPTURED: ContactKey/refresh for carol, after it lapsed. */
export const C1_REFRESH_CAROL_NOT_FOUND: ContactKeyRefreshResult = {
  "addresses": [
    {
      "address": "carol@example.org",
      "outcome": "none"
    }
  ],
  "checkedAt": "2026-10-21T18:25:43Z",
  "id": "01M5FV7A03Y8S9EB70K40ACJPW",
  "key": {
    "addresses": [
      "carol@example.org"
    ],
    "authenticated": false,
    "contactId": "01M5FV79ZK6YE48J5PZH0P42WB",
    "createdAt": "2026-10-21T18:22:41Z",
    "expired": true,
    "expiresAt": "2026-10-21T18:25:11Z",
    "expiringSoon": false,
    "fetchedAt": "2026-10-21T18:22:51Z",
    "fingerprint": "BAB603A8AAE478F90E9ACAED23DC15553303AC5F",
    "hasEncryptionKey": true,
    "id": "01M5FV7A03Y8S9EB70K40ACJPW",
    "kind": "pgp",
    "pinned": true,
    "pinnedFor": [
      "carol@example.org"
    ],
    "refused": false,
    "removedAt": null,
    "revoked": false,
    "source": "manual",
    "usable": false,
    "userIds": [
      {
        "email": "carol@example.org",
        "name": "Carol Example",
        "primary": true,
        "revoked": false
      }
    ]
  },
  "result": "notFound"
};

/** DERIVED from C1_REFRESH_CAROL_NOT_FOUND: a renewal of carol's key was served
 * and merged (result renewed; the key's expiry moved to 3 Nov 2028 and it is
 * usable again; the address outcome found). */
export const C1_REFRESH_CAROL_RENEWED: ContactKeyRefreshResult = {
  ...C1_REFRESH_CAROL_NOT_FOUND,
  result: "renewed",
  addresses: [{ address: "carol@example.org", outcome: "found" }],
  key: { ...C1_REFRESH_CAROL_NOT_FOUND.key, expired: false, expiresAt: "2028-11-03T18:25:11Z", usable: true },
};

/** DERIVED: the same key served, adding nothing (result unchanged). */
export const C1_REFRESH_ALICE_UNCHANGED: ContactKeyRefreshResult = {
  ...C1_REFRESH_ALICE_NOT_FOUND,
  result: "unchanged",
  addresses: [{ address: "alice@example.org", outcome: "found" }],
};

/** DERIVED: a different usable key served for alice (result differentKey;
 * newKeyId and newFingerprint name it — here bob's captured key stands in for
 * the stranger's, since only its fingerprint is read). */
export const C1_REFRESH_ALICE_DIFFERENT: ContactKeyRefreshResult = {
  ...C1_REFRESH_ALICE_NOT_FOUND,
  result: "differentKey",
  addresses: [{ address: "alice@example.org", outcome: "found" }],
  newKeyId: C1_CONTACT_KEY_BOB_REMOVED.id,
  newFingerprint: C1_CONTACT_KEY_BOB_REMOVED.fingerprint,
};

/** DERIVED: the directory could not be reached (result unreachable). */
export const C1_REFRESH_ALICE_UNREACHABLE: ContactKeyRefreshResult = {
  ...C1_REFRESH_ALICE_NOT_FOUND,
  result: "unreachable",
  addresses: [{ address: "alice@example.org", outcome: "failed" }],
};
