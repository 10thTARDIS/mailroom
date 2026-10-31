// SPDX-License-Identifier: AGPL-3.0-only

// Importing a correspondent's public key (#47, ADR-038): the parts of it that are
// not UI. The server (ContactKey/import) does the real reading of the key; this
// file turns what a person hands over (pasted text, a chosen file) into the one
// string the method takes, and turns the server's refusals into a closed set the
// dialog words in plain language.

import { JmapMethodError, JmapTransportError } from "./jmap/client";

/** The server's cap on a key (decoded). A file over twice this is not read. */
export const MAX_KEY_BYTES = 256 * 1024;

/** Every way an import can fail, as the dialog words them. `empty` is the dialog's
 * own (nothing pasted); `unavailable` is a server that cannot import or cannot be
 * reached; `failed` is anything else. */
export type ImportKeyError =
  | "empty"
  | "secretKeyRefused"
  | "keyDoesNotNameAddress"
  | "noEncryptionSubkey"
  | "keyExpiredOrRevoked"
  | "invalidKey"
  | "tooLarge"
  | "ownAddress"
  | "unavailable"
  | "failed";

/** What the dialog gets back: the key's identity, or why not. */
export type ImportKeyOutcome =
  | { ok: true; fingerprint: string; userId: string }
  | { ok: false; error: ImportKeyError };

const KNOWN: readonly ImportKeyError[] = [
  "secretKeyRefused",
  "keyDoesNotNameAddress",
  "noEncryptionSubkey",
  "keyExpiredOrRevoked",
  "invalidKey",
  "tooLarge",
  "ownAddress",
];

/** Map a thrown error to the closed set. An unknown method-error type is `failed`,
 * never one of the specific reasons: the dialog must not claim a cause it was not
 * told. */
export function importKeyErrorOf(err: unknown): ImportKeyError {
  if (err instanceof JmapMethodError) {
    return (KNOWN as readonly string[]).includes(err.type) ? (err.type as ImportKeyError) : "failed";
  }
  if (err instanceof JmapTransportError) return "unavailable";
  return "failed";
}

/**
 * The `publicKey` argument for a file's bytes. An armored key (text starting with
 * `-----BEGIN`) is passed as the text it is; anything else is a binary key, which
 * JSON cannot carry, so it is sent as base64 (the form the method accepts).
 */
export function keyFileToText(bytes: Uint8Array): string {
  const text = new TextDecoder("utf-8", { fatal: false }).decode(bytes).trim();
  if (text.startsWith("-----BEGIN")) return text;
  let bin = "";
  for (let i = 0; i < bytes.length; i += 0x8000) {
    bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  }
  return btoa(bin);
}
