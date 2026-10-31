// SPDX-License-Identifier: AGPL-3.0-only

// REAL RESPONSES FROM THE #828 SERVER (v1.94 contacts, ADR-041), not hand-made
// shapes. Every export marked CAPTURED was returned by the contacts surface
// running in the accessibility lane's stack (scripts/a11y-e2e.sh: a dev-auth
// instance over the lane's real Dovecot mailbox), driven over JMAP once by a
// capture script for this increment on 2026-10-22T04:36:15.669Z. The mailbox is the lane's own:
// Grace Hopper was saved by ingest because the fixture's sent message went to
// her; Ada was saved by hand, named "Countess Ada" and given notes; the
// Grand Theatre's list address was blocked to Trash and then merged into Ada
// and separated again; alice@example.org exists because a key was imported for
// her, and was merged into Alan to move the key.
//
// Ids, states and dates are as the server returned them. Anything DERIVED is
// built from a captured object with the change named beside it.
//
// Imported only by tests (the `.testdata` suffix keeps it out of the bundle).

import type { Contact, ContactMergeResult, ContactSeparateResult, QueryResult, SetResult } from "./jmap/types";

/** When the capture ran. */
export const CONTACTS_CAPTURED_AT = new Date("2026-10-22T04:36:15.669Z");

/** CAPTURED: Contact/get for Ada after Contact/set {isSaved:true}, {userName:"  Countess Ada  "} (trimmed by the server) and {notes} with a CRLF (stored as LF). */
export const ADA_NAMED: Contact = {
  "addresses": [
    {
      "address": "ada@example.com",
      "id": "01M5GYA96GBQWF3MN8QB1N0KP7",
      "reachedMeAt": "reader@example.com"
    }
  ],
  "blockDestination": "junk",
  "blockedAt": null,
  "displayName": "Ada Lovelace",
  "firstMailAt": "2026-10-22T04:36:09Z",
  "id": "01M5GYA96GBQWF3MN8Q7PR9T1M",
  "inboundCount": 4,
  "isBlocked": false,
  "isSaved": true,
  "isVip": false,
  "keys": [],
  "lastMailAt": "2026-10-22T04:36:09Z",
  "name": "Countess Ada",
  "notes": "Met at the\nanalytical engine talk.",
  "outboundCount": 0,
  "savedAt": "2026-10-22T04:36:16Z",
  "userName": "Countess Ada"
};

/** CAPTURED: Ada after Contact/set {userName: null} ("Use theirs"): name is the learned displayName again. */
export const ADA_THEIRS: Contact = {
  "addresses": [
    {
      "address": "ada@example.com",
      "id": "01M5GYA96GBQWF3MN8QB1N0KP7",
      "reachedMeAt": "reader@example.com"
    }
  ],
  "blockDestination": "junk",
  "blockedAt": null,
  "displayName": "Ada Lovelace",
  "firstMailAt": "2026-10-22T04:36:09Z",
  "id": "01M5GYA96GBQWF3MN8Q7PR9T1M",
  "inboundCount": 4,
  "isBlocked": false,
  "isSaved": true,
  "isVip": false,
  "keys": [],
  "lastMailAt": "2026-10-22T04:36:09Z",
  "name": "Ada Lovelace",
  "notes": "Met at the\nanalytical engine talk.",
  "outboundCount": 0,
  "savedAt": "2026-10-22T04:36:16Z",
  "userName": null
};

/** CAPTURED: Grace, saved by ingest (the fixture's sent message went to her), after Contact/set {isVip:true}. */
export const GRACE_VIP: Contact = {
  "addresses": [
    {
      "address": "grace@example.com",
      "id": "01M5GYA9414TEFMR9ANHDZ4P0C",
      "reachedMeAt": "reader@example.com"
    }
  ],
  "blockDestination": "junk",
  "blockedAt": null,
  "displayName": "Grace Hopper",
  "firstMailAt": "2026-10-22T04:36:09Z",
  "id": "01M5GYA9414TEFMR9ANG5QTH8E",
  "inboundCount": 3,
  "isBlocked": false,
  "isSaved": true,
  "isVip": true,
  "keys": [],
  "lastMailAt": "2026-10-22T04:36:09Z",
  "name": "Grace Hopper",
  "notes": "",
  "outboundCount": 1,
  "savedAt": "2026-10-22T04:36:09Z",
  "userName": null
};

/** CAPTURED: Grace after Contact/set {isSaved:false} while a VIP. THE SERVER KEEPS VIP: isSaved false, isVip still true. */
export const GRACE_VIP_UNSAVED: Contact = {
  "addresses": [
    {
      "address": "grace@example.com",
      "id": "01M5GYA9414TEFMR9ANHDZ4P0C",
      "reachedMeAt": "reader@example.com"
    }
  ],
  "blockDestination": "junk",
  "blockedAt": null,
  "displayName": "Grace Hopper",
  "firstMailAt": "2026-10-22T04:36:09Z",
  "id": "01M5GYA9414TEFMR9ANG5QTH8E",
  "inboundCount": 3,
  "isBlocked": false,
  "isSaved": false,
  "isVip": true,
  "keys": [],
  "lastMailAt": "2026-10-22T04:36:09Z",
  "name": "Grace Hopper",
  "notes": "",
  "outboundCount": 1,
  "savedAt": null,
  "userName": null
};

/** CAPTURED: the Grand Theatre's list address after Contact/set {isBlocked:true, blockDestination:"trash"}: blocked, never saved. */
export const NEWS_BLOCKED: Contact = {
  "addresses": [
    {
      "address": "updates@news.theatre.example.com",
      "id": "01M5GYA9BTYXA3962912KVC5QS",
      "reachedMeAt": "reader@example.com"
    }
  ],
  "blockDestination": "trash",
  "blockedAt": "2026-10-22T04:36:16Z",
  "displayName": "The Grand Theatre",
  "firstMailAt": "2026-10-22T04:36:09Z",
  "id": "01M5GYA9BTYXA3962910TPXPMY",
  "inboundCount": 1,
  "isBlocked": true,
  "isSaved": false,
  "isVip": false,
  "keys": [],
  "lastMailAt": "2026-10-22T04:36:09Z",
  "name": "The Grand Theatre",
  "notes": "",
  "outboundCount": 0,
  "savedAt": null,
  "userName": null
};

/** CAPTURED: Ada just before the merge (named, notes, one address, not blocked). */
export const ADA_PRE_MERGE: Contact = {
  "addresses": [
    {
      "address": "ada@example.com",
      "id": "01M5GYA96GBQWF3MN8QB1N0KP7",
      "reachedMeAt": "reader@example.com"
    }
  ],
  "blockDestination": "junk",
  "blockedAt": null,
  "displayName": "Ada Lovelace",
  "firstMailAt": "2026-10-22T04:36:09Z",
  "id": "01M5GYA96GBQWF3MN8Q7PR9T1M",
  "inboundCount": 4,
  "isBlocked": false,
  "isSaved": true,
  "isVip": false,
  "keys": [],
  "lastMailAt": "2026-10-22T04:36:09Z",
  "name": "Countess Ada",
  "notes": "Met at the\nanalytical engine talk.",
  "outboundCount": 0,
  "savedAt": "2026-10-22T04:36:16Z",
  "userName": "Countess Ada"
};

/** CAPTURED: Ada after Contact/merge {into: Ada, from: Grand Theatre}: two addresses, and the theatre's block CARRIED OVER (isBlocked, trash, its blockedAt). */
export const ADA_MERGED: Contact = {
  "addresses": [
    {
      "address": "ada@example.com",
      "id": "01M5GYA96GBQWF3MN8QB1N0KP7",
      "reachedMeAt": "reader@example.com"
    },
    {
      "address": "updates@news.theatre.example.com",
      "id": "01M5GYA9BTYXA3962912KVC5QS",
      "reachedMeAt": "reader@example.com"
    }
  ],
  "blockDestination": "trash",
  "blockedAt": "2026-10-22T04:36:16Z",
  "displayName": "Ada Lovelace",
  "firstMailAt": "2026-10-22T04:36:09Z",
  "id": "01M5GYA96GBQWF3MN8Q7PR9T1M",
  "inboundCount": 5,
  "isBlocked": true,
  "isSaved": true,
  "isVip": false,
  "keys": [],
  "lastMailAt": "2026-10-22T04:36:09Z",
  "name": "Countess Ada",
  "notes": "Met at the\nanalytical engine talk.",
  "outboundCount": 0,
  "savedAt": "2026-10-22T04:36:16Z",
  "userName": "Countess Ada"
};

/** CAPTURED: Ada after Contact/separate of the theatre's address: one address again; the block stays (separating never unblocks).
 * Captured BEFORE #834: a server with merge records restores Ada's own (unblocked) state, because the block was only the theatre's. */
export const ADA_SEPARATED: Contact = {
  "addresses": [
    {
      "address": "ada@example.com",
      "id": "01M5GYA96GBQWF3MN8QB1N0KP7",
      "reachedMeAt": "reader@example.com"
    }
  ],
  "blockDestination": "trash",
  "blockedAt": "2026-10-22T04:36:16Z",
  "displayName": "Ada Lovelace",
  "firstMailAt": "2026-10-22T04:36:09Z",
  "id": "01M5GYA96GBQWF3MN8Q7PR9T1M",
  "inboundCount": 4,
  "isBlocked": true,
  "isSaved": true,
  "isVip": false,
  "keys": [],
  "lastMailAt": "2026-10-22T04:36:09Z",
  "name": "Countess Ada",
  "notes": "Met at the\nanalytical engine talk.",
  "outboundCount": 0,
  "savedAt": "2026-10-22T04:36:16Z",
  "userName": "Countess Ada"
};

/** CAPTURED: Alan after Contact/merge {into: Alan, from: alice}: alice's address and key moved. */
export const ALAN_WITH_KEY: Contact = {
  "addresses": [
    {
      "address": "alan@example.com",
      "id": "01M5GYA9JCX8R6MSTB4WJYHJ1H",
      "reachedMeAt": "reader@example.com"
    },
    {
      "address": "alice@example.org",
      "id": "01M5GYAFJ57JB0B4AWPC93ERJ9",
      "reachedMeAt": null
    }
  ],
  "blockDestination": "junk",
  "blockedAt": null,
  "displayName": "Alan Turing",
  "firstMailAt": "2026-10-22T04:36:09Z",
  "id": "01M5GYA9JCX8R6MSTB4RPHDY0Z",
  "inboundCount": 2,
  "isBlocked": false,
  "isSaved": true,
  "isVip": false,
  "keys": [
    {
      "fetchedAt": "2026-10-22T04:36:16Z",
      "fingerprint": "0676835ABFFF3AAA06C6377A39CC7A5A5C40CC80",
      "kind": "pgp",
      "refused": false,
      "source": "manual"
    }
  ],
  "lastMailAt": "2026-10-22T04:36:16Z",
  "name": "Alan Turing",
  "notes": "",
  "outboundCount": 0,
  "savedAt": "2026-10-22T04:36:16Z",
  "userName": null
};

/** CAPTURED: Contact/set {isVip:true, isSaved:false} in one patch: refused. */
export const SET_VIP_AND_UNSAVE = {
  "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
  "oldState": "s:14",
  "newState": "s:14",
  "created": {},
  "updated": {},
  "destroyed": [],
  "notCreated": {},
  "notUpdated": {
    "01M5GYA9JCX8R6MSTB4RPHDY0Z": {
      "type": "invalidProperties",
      "description": "store: a contact cannot be marked VIP and removed from contacts in one change",
      "properties": [
        "isVip",
        "isSaved"
      ]
    }
  },
  "notDestroyed": {}
} as unknown as SetResult<Contact>;

/** CAPTURED: Contact/set {userName: 201 characters}: refused. */
export const SET_USERNAME_TOO_LONG = {
  "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
  "oldState": "s:15",
  "newState": "s:15",
  "created": {},
  "updated": {},
  "destroyed": [],
  "notCreated": {},
  "notUpdated": {
    "01M5GYA96GBQWF3MN8Q7PR9T1M": {
      "type": "invalidProperties",
      "description": "store: a contact name is at most 200 characters",
      "properties": [
        "userName"
      ]
    }
  },
  "notDestroyed": {}
} as unknown as SetResult<Contact>;

/** CAPTURED: Contact/set {notes: 4,001 characters}: refused. */
export const SET_NOTES_TOO_LONG = {
  "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
  "oldState": "s:16",
  "newState": "s:16",
  "created": {},
  "updated": {},
  "destroyed": [],
  "notCreated": {},
  "notUpdated": {
    "01M5GYA96GBQWF3MN8Q7PR9T1M": {
      "type": "invalidProperties",
      "description": "store: contact notes are at most 4000 characters",
      "properties": [
        "notes"
      ]
    }
  },
  "notDestroyed": {}
} as unknown as SetResult<Contact>;

/** CAPTURED: Contact/set {notes: exactly 4,000 characters}: accepted. */
export const SET_NOTES_4000 = {
  "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
  "oldState": "s:16",
  "newState": "s:17",
  "created": {},
  "updated": {
    "01M5GYA9JCX8R6MSTB4RPHDY0Z": null
  },
  "destroyed": [],
  "notCreated": {},
  "notUpdated": {},
  "notDestroyed": {}
} as unknown as SetResult<Contact>;

/** CAPTURED: Contact/set {isSaved:true} on an unknown id. */
export const SET_NOT_FOUND = {
  "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
  "oldState": "s:18",
  "newState": "s:18",
  "created": {},
  "updated": {},
  "destroyed": [],
  "notCreated": {},
  "notUpdated": {
    "01ZZZZZZZZZZZZZZZZZZZZZZZZ": {
      "type": "notFound"
    }
  },
  "notDestroyed": {}
} as unknown as SetResult<Contact>;

/** CAPTURED: Contact/set {isSaved:false} on the VIP Grace: accepted. */
export const SET_UNSAVE_VIP = {
  "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
  "oldState": "s:20",
  "newState": "s:21",
  "created": {},
  "updated": {
    "01M5GYA9414TEFMR9ANG5QTH8E": null
  },
  "destroyed": [],
  "notCreated": {},
  "notUpdated": {},
  "notDestroyed": {}
} as unknown as SetResult<Contact>;

/** CAPTURED: Contact/query {isSaved:true} sorted by name (ascending), limit 50, with its Contact/get. */
export const Q_SAVED_BY_NAME = {
  "query": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "queryState": "s:23",
    "canCalculateChanges": false,
    "position": 0,
    "total": 2,
    "ids": [
      "01M5GYAFJ57JB0B4AWPAM9SMF0",
      "01M5GYA96GBQWF3MN8Q7PR9T1M"
    ]
  },
  "get": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "state": "s:23",
    "list": [
      {
        "addresses": [
          {
            "address": "alice@example.org",
            "id": "01M5GYAFJ57JB0B4AWPC93ERJ9",
            "reachedMeAt": null
          }
        ],
        "blockDestination": "junk",
        "blockedAt": null,
        "displayName": "",
        "firstMailAt": "2026-10-22T04:36:16Z",
        "id": "01M5GYAFJ57JB0B4AWPAM9SMF0",
        "inboundCount": 1,
        "isBlocked": false,
        "isSaved": true,
        "isVip": false,
        "keys": [
          {
            "fetchedAt": "2026-10-22T04:36:16Z",
            "fingerprint": "0676835ABFFF3AAA06C6377A39CC7A5A5C40CC80",
            "kind": "pgp",
            "refused": false,
            "source": "manual"
          }
        ],
        "lastMailAt": "2026-10-22T04:36:16Z",
        "name": "",
        "notes": "",
        "outboundCount": 0,
        "savedAt": "2026-10-22T04:36:16Z",
        "userName": null
      },
      {
        "addresses": [
          {
            "address": "ada@example.com",
            "id": "01M5GYA96GBQWF3MN8QB1N0KP7",
            "reachedMeAt": "reader@example.com"
          }
        ],
        "blockDestination": "junk",
        "blockedAt": null,
        "displayName": "Ada Lovelace",
        "firstMailAt": "2026-10-22T04:36:09Z",
        "id": "01M5GYA96GBQWF3MN8Q7PR9T1M",
        "inboundCount": 4,
        "isBlocked": false,
        "isSaved": true,
        "isVip": false,
        "keys": [],
        "lastMailAt": "2026-10-22T04:36:09Z",
        "name": "Countess Ada",
        "notes": "Met at the\nanalytical engine talk.",
        "outboundCount": 0,
        "savedAt": "2026-10-22T04:36:16Z",
        "userName": "Countess Ada"
      }
    ],
    "notFound": []
  }
} as unknown as { query: QueryResult; get: { list: Contact[] } };

/** CAPTURED: the same, {sort: lastMailAt, isAscending:false}. */
export const Q_SAVED_BY_LAST = {
  "query": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "queryState": "s:23",
    "canCalculateChanges": false,
    "position": 0,
    "total": 2,
    "ids": [
      "01M5GYAFJ57JB0B4AWPAM9SMF0",
      "01M5GYA96GBQWF3MN8Q7PR9T1M"
    ]
  },
  "get": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "state": "s:23",
    "list": [
      {
        "addresses": [
          {
            "address": "alice@example.org",
            "id": "01M5GYAFJ57JB0B4AWPC93ERJ9",
            "reachedMeAt": null
          }
        ],
        "blockDestination": "junk",
        "blockedAt": null,
        "displayName": "",
        "firstMailAt": "2026-10-22T04:36:16Z",
        "id": "01M5GYAFJ57JB0B4AWPAM9SMF0",
        "inboundCount": 1,
        "isBlocked": false,
        "isSaved": true,
        "isVip": false,
        "keys": [
          {
            "fetchedAt": "2026-10-22T04:36:16Z",
            "fingerprint": "0676835ABFFF3AAA06C6377A39CC7A5A5C40CC80",
            "kind": "pgp",
            "refused": false,
            "source": "manual"
          }
        ],
        "lastMailAt": "2026-10-22T04:36:16Z",
        "name": "",
        "notes": "",
        "outboundCount": 0,
        "savedAt": "2026-10-22T04:36:16Z",
        "userName": null
      },
      {
        "addresses": [
          {
            "address": "ada@example.com",
            "id": "01M5GYA96GBQWF3MN8QB1N0KP7",
            "reachedMeAt": "reader@example.com"
          }
        ],
        "blockDestination": "junk",
        "blockedAt": null,
        "displayName": "Ada Lovelace",
        "firstMailAt": "2026-10-22T04:36:09Z",
        "id": "01M5GYA96GBQWF3MN8Q7PR9T1M",
        "inboundCount": 4,
        "isBlocked": false,
        "isSaved": true,
        "isVip": false,
        "keys": [],
        "lastMailAt": "2026-10-22T04:36:09Z",
        "name": "Countess Ada",
        "notes": "Met at the\nanalytical engine talk.",
        "outboundCount": 0,
        "savedAt": "2026-10-22T04:36:16Z",
        "userName": "Countess Ada"
      }
    ],
    "notFound": []
  }
} as unknown as { query: QueryResult; get: { list: Contact[] } };

/** CAPTURED: page 1 of {isSaved:true} at limit 1: total 2. */
export const Q_SAVED_PAGE1 = {
  "query": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "queryState": "s:23",
    "canCalculateChanges": false,
    "position": 0,
    "total": 2,
    "ids": [
      "01M5GYAFJ57JB0B4AWPAM9SMF0"
    ]
  },
  "get": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "state": "s:23",
    "list": [
      {
        "addresses": [
          {
            "address": "alice@example.org",
            "id": "01M5GYAFJ57JB0B4AWPC93ERJ9",
            "reachedMeAt": null
          }
        ],
        "blockDestination": "junk",
        "blockedAt": null,
        "displayName": "",
        "firstMailAt": "2026-10-22T04:36:16Z",
        "id": "01M5GYAFJ57JB0B4AWPAM9SMF0",
        "inboundCount": 1,
        "isBlocked": false,
        "isSaved": true,
        "isVip": false,
        "keys": [
          {
            "fetchedAt": "2026-10-22T04:36:16Z",
            "fingerprint": "0676835ABFFF3AAA06C6377A39CC7A5A5C40CC80",
            "kind": "pgp",
            "refused": false,
            "source": "manual"
          }
        ],
        "lastMailAt": "2026-10-22T04:36:16Z",
        "name": "",
        "notes": "",
        "outboundCount": 0,
        "savedAt": "2026-10-22T04:36:16Z",
        "userName": null
      }
    ],
    "notFound": []
  }
} as unknown as { query: QueryResult; get: { list: Contact[] } };

/** CAPTURED: page 2 (position 1, limit 1). */
export const Q_SAVED_PAGE2 = {
  "query": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "queryState": "s:23",
    "canCalculateChanges": false,
    "position": 1,
    "total": 2,
    "ids": [
      "01M5GYA96GBQWF3MN8Q7PR9T1M"
    ]
  },
  "get": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "state": "s:23",
    "list": [
      {
        "addresses": [
          {
            "address": "ada@example.com",
            "id": "01M5GYA96GBQWF3MN8QB1N0KP7",
            "reachedMeAt": "reader@example.com"
          }
        ],
        "blockDestination": "junk",
        "blockedAt": null,
        "displayName": "Ada Lovelace",
        "firstMailAt": "2026-10-22T04:36:09Z",
        "id": "01M5GYA96GBQWF3MN8Q7PR9T1M",
        "inboundCount": 4,
        "isBlocked": false,
        "isSaved": true,
        "isVip": false,
        "keys": [],
        "lastMailAt": "2026-10-22T04:36:09Z",
        "name": "Countess Ada",
        "notes": "Met at the\nanalytical engine talk.",
        "outboundCount": 0,
        "savedAt": "2026-10-22T04:36:16Z",
        "userName": "Countess Ada"
      }
    ],
    "notFound": []
  }
} as unknown as { query: QueryResult; get: { list: Contact[] } };

/** CAPTURED: Contact/query {isBlocked:true} (no isSaved): the theatre, never saved. */
export const Q_BLOCKED = {
  "query": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "queryState": "s:23",
    "canCalculateChanges": false,
    "position": 0,
    "total": 1,
    "ids": [
      "01M5GYA9BTYXA3962910TPXPMY"
    ]
  },
  "get": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "state": "s:23",
    "list": [
      {
        "addresses": [
          {
            "address": "updates@news.theatre.example.com",
            "id": "01M5GYA9BTYXA3962912KVC5QS",
            "reachedMeAt": "reader@example.com"
          }
        ],
        "blockDestination": "trash",
        "blockedAt": "2026-10-22T04:36:16Z",
        "displayName": "The Grand Theatre",
        "firstMailAt": "2026-10-22T04:36:09Z",
        "id": "01M5GYA9BTYXA3962910TPXPMY",
        "inboundCount": 1,
        "isBlocked": true,
        "isSaved": false,
        "isVip": false,
        "keys": [],
        "lastMailAt": "2026-10-22T04:36:09Z",
        "name": "The Grand Theatre",
        "notes": "",
        "outboundCount": 0,
        "savedAt": null,
        "userName": null
      }
    ],
    "notFound": []
  }
} as unknown as { query: QueryResult; get: { list: Contact[] } };

/** CAPTURED: Contact/query {isVip:true} (no isSaved) after Grace was un-saved: she is still listed. */
export const Q_VIP_ANY = {
  "query": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "queryState": "s:23",
    "canCalculateChanges": false,
    "position": 0,
    "total": 1,
    "ids": [
      "01M5GYA9414TEFMR9ANG5QTH8E"
    ]
  },
  "get": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "state": "s:23",
    "list": [
      {
        "addresses": [
          {
            "address": "grace@example.com",
            "id": "01M5GYA9414TEFMR9ANHDZ4P0C",
            "reachedMeAt": "reader@example.com"
          }
        ],
        "blockDestination": "junk",
        "blockedAt": null,
        "displayName": "Grace Hopper",
        "firstMailAt": "2026-10-22T04:36:09Z",
        "id": "01M5GYA9414TEFMR9ANG5QTH8E",
        "inboundCount": 3,
        "isBlocked": false,
        "isSaved": false,
        "isVip": true,
        "keys": [],
        "lastMailAt": "2026-10-22T04:36:09Z",
        "name": "Grace Hopper",
        "notes": "",
        "outboundCount": 1,
        "savedAt": null,
        "userName": null
      }
    ],
    "notFound": []
  }
} as unknown as { query: QueryResult; get: { list: Contact[] } };

/** CAPTURED: Contact/query {isSaved:true, isVip:true} at the same moment: EMPTY. The un-saved VIP is invisible to it. */
export const Q_VIP_SAVED = {
  "query": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "queryState": "s:23",
    "canCalculateChanges": false,
    "position": 0,
    "total": 0,
    "ids": []
  },
  "get": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "state": "s:23",
    "list": [],
    "notFound": []
  }
} as unknown as { query: QueryResult; get: { list: Contact[] } };

/** CAPTURED: Contact/query {isSaved:true, hasKey:true}. */
export const Q_SAVED_HAS_KEY = {
  "query": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "queryState": "s:23",
    "canCalculateChanges": false,
    "position": 0,
    "total": 1,
    "ids": [
      "01M5GYAFJ57JB0B4AWPAM9SMF0"
    ]
  },
  "get": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "state": "s:23",
    "list": [
      {
        "addresses": [
          {
            "address": "alice@example.org",
            "id": "01M5GYAFJ57JB0B4AWPC93ERJ9",
            "reachedMeAt": null
          }
        ],
        "blockDestination": "junk",
        "blockedAt": null,
        "displayName": "",
        "firstMailAt": "2026-10-22T04:36:16Z",
        "id": "01M5GYAFJ57JB0B4AWPAM9SMF0",
        "inboundCount": 1,
        "isBlocked": false,
        "isSaved": true,
        "isVip": false,
        "keys": [
          {
            "fetchedAt": "2026-10-22T04:36:16Z",
            "fingerprint": "0676835ABFFF3AAA06C6377A39CC7A5A5C40CC80",
            "kind": "pgp",
            "refused": false,
            "source": "manual"
          }
        ],
        "lastMailAt": "2026-10-22T04:36:16Z",
        "name": "",
        "notes": "",
        "outboundCount": 0,
        "savedAt": "2026-10-22T04:36:16Z",
        "userName": null
      }
    ],
    "notFound": []
  }
} as unknown as { query: QueryResult; get: { list: Contact[] } };

/** CAPTURED: Contact/query {address: "alan@example.com"} (any contact, saved or not). */
export const Q_BY_ADDRESS = {
  "query": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "queryState": "s:23",
    "canCalculateChanges": false,
    "position": 0,
    "total": 1,
    "ids": [
      "01M5GYA9JCX8R6MSTB4RPHDY0Z"
    ]
  },
  "get": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "state": "s:23",
    "list": [
      {
        "addresses": [
          {
            "address": "alan@example.com",
            "id": "01M5GYA9JCX8R6MSTB4WJYHJ1H",
            "reachedMeAt": "reader@example.com"
          }
        ],
        "blockDestination": "junk",
        "blockedAt": null,
        "displayName": "Alan Turing",
        "firstMailAt": "2026-10-22T04:36:09Z",
        "id": "01M5GYA9JCX8R6MSTB4RPHDY0Z",
        "inboundCount": 1,
        "isBlocked": false,
        "isSaved": false,
        "isVip": false,
        "keys": [],
        "lastMailAt": "2026-10-22T04:36:09Z",
        "name": "Alan Turing",
        "notes": "",
        "outboundCount": 0,
        "savedAt": null,
        "userName": null
      }
    ],
    "notFound": []
  }
} as unknown as { query: QueryResult; get: { list: Contact[] } };

/** CAPTURED: Contact/query {address} for an address nobody has written from or to. */
export const Q_BY_ADDRESS_NONE = {
  "query": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "queryState": "s:23",
    "canCalculateChanges": false,
    "position": 0,
    "total": 0,
    "ids": []
  },
  "get": {
    "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
    "state": "s:23",
    "list": [],
    "notFound": []
  }
} as unknown as { query: QueryResult; get: { list: Contact[] } };

/** CAPTURED: Contact/merge {into: Ada, from: Grand Theatre}. */
export const MERGE_RESULT = {
  "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
  "oldState": "s:23",
  "newState": "s:25",
  "id": "01M5GYA96GBQWF3MN8Q7PR9T1M",
  "destroyedId": "01M5GYA9BTYXA3962910TPXPMY",
  "addressesMoved": 1,
  "keysMoved": 0,
  "keysCombined": 0
} as unknown as ContactMergeResult;

/** CAPTURED: Contact/separate {id: Ada, addressIds: [the theatre's]}. */
export const SEPARATE_RESULT = {
  "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
  "oldState": "s:25",
  "newState": "s:27",
  "id": "01M5GYA96GBQWF3MN8Q7PR9T1M",
  "created": [
    "01M5GYAFZX8D98A4F3B8YQ0J2C"
  ],
  "addressContacts": {
    "01M5GYA9BTYXA3962912KVC5QS": "01M5GYAFZX8D98A4F3B8YQ0J2C"
  },
  "keysMoved": 0
} as unknown as ContactSeparateResult;

/** DERIVED from SEPARATE_RESULT: the two fields #834 added (internal/api/contact_merge.go
 * contactSeparateResult), for a separate that undid the merge but kept the user's later notes. */
export const SEPARATE_RESULT_KEPT_NOTES = {
  ...SEPARATE_RESULT,
  "restored": true,
  "keptFields": ["notes"],
} as unknown as ContactSeparateResult;

/** DERIVED from SEPARATE_RESULT: a partial separate (#834), which restores nothing. */
export const SEPARATE_RESULT_PARTIAL = {
  ...SEPARATE_RESULT,
  "restored": false,
  "keptFields": [],
} as unknown as ContactSeparateResult;

/** CAPTURED: the new contact Contact/separate made (blocked, from the theatre's own state). */
export const SEPARATED_NEW: Contact = {
  "addresses": [
    {
      "address": "updates@news.theatre.example.com",
      "id": "01M5GYA9BTYXA3962912KVC5QS",
      "reachedMeAt": "reader@example.com"
    }
  ],
  "blockDestination": "trash",
  "blockedAt": "2026-10-22T04:36:16Z",
  "displayName": "The Grand Theatre",
  "firstMailAt": "2026-10-22T04:36:09Z",
  "id": "01M5GYAFZX8D98A4F3B8YQ0J2C",
  "inboundCount": 1,
  "isBlocked": true,
  "isSaved": false,
  "isVip": false,
  "keys": [],
  "lastMailAt": "2026-10-22T04:36:09Z",
  "name": "The Grand Theatre",
  "notes": "",
  "outboundCount": 0,
  "savedAt": null,
  "userName": null
};

/** CAPTURED: the method error for mergeSelf. */
export const ERR_MERGE_SELF = {
  "type": "invalidArguments",
  "description": "store: a contact cannot be merged with itself"
} as { type: string; description?: string };

/** CAPTURED: the method error for mergeNotFound. */
export const ERR_MERGE_NOT_FOUND = {
  "type": "notFound",
  "description": "no such contact"
} as { type: string; description?: string };

/** CAPTURED: the method error for mergeStale. */
export const ERR_MERGE_STALE = {
  "type": "stateMismatch"
} as { type: string; description?: string };

/** CAPTURED: the method error for separateEmpty. */
export const ERR_SEPARATE_EMPTY = {
  "type": "invalidArguments",
  "description": "addressIds must name at least one address"
} as { type: string; description?: string };

/** CAPTURED: the method error for separateAll. */
export const ERR_SEPARATE_ALL = {
  "type": "invalidArguments",
  "description": "store: a contact keeps at least one address; separate the others"
} as { type: string; description?: string };

/** CAPTURED: the method error for separateForeign. */
export const ERR_SEPARATE_FOREIGN = {
  "type": "invalidArguments",
  "description": "store: that address is not one of the contact's"
} as { type: string; description?: string };

/** CAPTURED: Email/query {query: "from:ada@example.com OR to:ada@example.com"}: the recent-mail search over the lane's mailbox. */
export const RECENT_QUERY = {
  "accountId": "01M5GYA8MQJDW65BP0AYE1G01R",
  "queryState": "s:24",
  "canCalculateChanges": false,
  "position": 0,
  "total": 4,
  "ids": [
    "01M5GYA9JTE8W66QC7FRV1D6GV",
    "01M5GYA9GXC4R2MNTJAB6MBTHE",
    "01M5GYA9G75YKZP21MJV1KN9GN",
    "01M5GYA96A1EWPJFY6F03CEEXF"
  ]
} as unknown as QueryResult;
