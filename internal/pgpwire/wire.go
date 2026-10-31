// SPDX-License-Identifier: AGPL-3.0-only

// Package pgpwire is the protocol spoken between the main application and the
// crypto process that holds PGP private keys (ADR-024).
//
// It is deliberately the ONLY thing the two sides share. The application
// imports this package to make calls; the daemon (internal/pgpkeyd) imports it
// to serve them. Neither imports the other, so no code path in the application
// can reach the key material handling by accident.
//
// # Transport
//
// HTTP/1.1 over a Unix domain socket, JSON bodies, POST /v1/<method>, plus
// GET /healthz. HTTP rather than a bespoke framing because net/http already
// brings request size limits, read/write timeouts and context cancellation,
// which a hand-rolled length-prefixed protocol would have to re-earn — and the
// socket is not on a network, so none of HTTP's networked attack surface
// applies. Who may connect at all is a kernel decision: the socket is created
// 0600 and owned by the runtime user.
//
// # The API is PGP-shaped on purpose
//
// S/MIME is deferred to 2.x (Forgejo #69) on measured evidence — zero S/MIME
// messages and zero S/MIME correspondents across 940 examined — so these names
// say OpenPGP rather than carrying an algorithm-neutral abstraction with
// exactly one implementation. Reversing that later is an interface change:
// a known cost, recorded in ADR-024, not a surprise.
//
// # What is deliberately absent
//
// There is no export-private, and there never will be: no method returns a
// private key this process could itself open.
//
// export-wrapped EXISTS as of #54, and the distinction it draws is the whole
// design. ADR-024 §4 deferred it "until something needs it"; the need arrived
// when a hard delete became real, because deleting a GENERATED key destroys the
// only copy of the private half and retroactively makes mail the user already
// exported unreadable. What it returns is encrypted under a passphrase THE USER
// CHOOSES — a string this process never stores and cannot derive. Wrapping under
// the process's own root would have satisfied the spec's wording and delivered
// no portability at all.
package pgpwire

import (
	"fmt"
	"time"
)

// Method is one callable operation on the crypto process.
type Method string

// The methods that exist.
const (
	MethodGenerate     Method = "generate"
	MethodExportPublic Method = "export-public"
	// MethodImport takes an existing armored private key into custody. It is
	// the ONLY method whose REQUEST carries private key material, which makes
	// it the only one where the leak risk points inward — see ImportRequest.
	MethodImport Method = "import"
	// MethodBindIdentity makes a stored key the signing key for one of the
	// user's addresses.
	MethodBindIdentity Method = "bind-identity"
	// MethodSign produces a detached signature over caller-supplied bytes. It is
	// the first method that USES a private key rather than merely describing one,
	// which is the whole reason the boundary exists: the application gets a
	// signature, never the key that made it.
	MethodSign Method = "sign"
	// MethodExportWrapped returns the user's private key ENCRYPTED UNDER A
	// PASSPHRASE THEY CHOSE. It is spec §9's fifth operation, and the only one
	// whose RESPONSE carries private key material at all.
	//
	// ⚠ IT IS NOT export-private, AND THE DIFFERENCE IS THE WHOLE POINT. What
	// comes back is an OpenPGP key whose secret packets are encrypted with a
	// string this process never stores and cannot derive. Mailroom's own root
	// does not open it; only the person who supplied the passphrase can. Wrapping
	// it under the process's own secret instead would satisfy the spec's wording
	// and deliver ZERO portability — an exported file nothing outside this
	// deployment could read.
	//
	// ADR-024 §4 deferred building this "until something needs it". #54 is that:
	// a hard delete destroys the only copy of a generated key, and an export that
	// cannot carry the key leaves PGP-encrypted mail permanently unreadable.
	MethodExportWrapped Method = "export-wrapped"
	// MethodDecrypt opens one PGP/MIME encrypted part with the user's keys and
	// returns the decrypted MIME entity (#47 increment 6b, ADR-037).
	//
	// ⚠ IT IS THE ONE METHOD WHOSE RESPONSE IS PLAINTEXT MAIL, and that changes
	// the threat model's shape: user_id is ASSERTED by the caller (ADR-024 §4 as
	// amended), so an application compromise turns this socket into a decrypt
	// oracle for every user's encrypted history. Everything about it is shaped
	// by that, and by the 2026-08-21 checklist on #47:
	//
	//   - ONE opaque failure code (CodeDecryptFailed) for every cryptographic
	//     failure, with a fixed message. No go-crypto text, no MDC-versus-parse
	//     or quick-check distinction — the classic OpenPGP oracles.
	//   - SED (no MDC) is refused outright; integrity protection is mandatory.
	//   - its own request bound (MaxDecryptRequestBytes) and a hard cap on the
	//     DECOMPRESSED output (MaxDecryptedBytes).
	//   - a per-user rate limit, and a persistent audit row per call, written by
	//     the crypto process itself so a compromised application cannot skip it.
	//   - retired keys (decrypt_only, revoked) STILL DECRYPT. Retirement
	//     semantics invert compared with Sign: refusing them would orphan old
	//     mail silently.
	//   - a signature INSIDE the encryption (RFC 3156 section 6.2) is checked in
	//     the same read against candidate PUBLIC keys the caller sends, and
	//     reported as a status only (ADR-039 section 6). The caller decides what
	//     it means; a bad signature never withholds the plaintext.
	MethodDecrypt Method = "decrypt"
	// MethodEncrypt encrypts one MIME entity to the recipients' PUBLIC keys and to
	// the sender's own (#47, ADR-038).
	//
	// ⚠ IT USES NO PRIVATE KEY AT ALL. Encryption needs only public keys, so
	// unlike sign and decrypt this verb cannot lose a secret; it lives in the
	// crypto process so that every OpenPGP operation Mailroom performs is made by
	// one small, bounded, SEIPD-only, go-crypto-only implementation rather than a
	// second copy in the application. The signature is NOT made here: the
	// application signs first (sign, RFC 3156 multipart/signed) and hands that
	// signed entity to this verb, which is RFC 3156 §6.1's sign-then-encrypt.
	//
	//   - the sender's OWN public key is always added as a recipient, looked up by
	//     key id and ownership-checked like sign, so the Sent copy stays readable
	//     through decrypt-on-open;
	//   - every recipient key must carry a valid, unexpired, unrevoked encryption
	//     subkey, else the call is refused naming the ADDRESS (never key bytes);
	//   - SEIPD with an MDC only: there is no code path that writes a
	//     Symmetrically Encrypted Data packet;
	//   - bounded input (MaxEncryptInputBytes, which is below the decrypt cap so
	//     that what can be encrypted here can always be opened again).
	MethodEncrypt Method = "encrypt"
	// MethodExtend re-signs a stored key's self-signatures with a later expiry
	// (#795, ADR-040): the primary key's user-ID self-signatures and every live
	// subkey's binding. Same key, same fingerprint, so no correspondent sees
	// "key changed"; the refreshed public key reaches them by Autocrypt or WKD
	// and is merged into their copy. It needs the private half, which is why it
	// is here. A revoked key is refused.
	MethodExtend Method = "extend"
	// MethodRevoke adds a real OpenPGP key revocation signature to a stored key
	// and marks it revoked and decrypt-only (#795, ADR-040). Irreversible: a
	// revocation, once published, is merged into every copy that sees it. The
	// request must name the last 8 hex digits of the fingerprint, so a caller
	// that picked the wrong key id is refused rather than obeyed.
	MethodRevoke Method = "revoke"
)

// Methods is THE registry of served methods, and the reason it exists is
// testing rather than routing.
//
// The daemon builds its router from this list and refuses to start if its
// handler table does not match it exactly. The leak test ("no API path returns
// private key material") iterates this list and asserts that its own probe
// table covers precisely these entries. Between them, a method added without a
// probe fails the suite instead of being silently skipped — which is the way
// that test would otherwise rot into passing vacuously.
var Methods = []Method{
	MethodGenerate,
	MethodExportPublic,
	MethodImport,
	MethodBindIdentity,
	MethodSign,
	MethodExportWrapped,
	MethodDecrypt,
	MethodEncrypt,
	MethodExtend,
	MethodRevoke,
}

// Path is where a method is served.
func (m Method) Path() string { return "/v1/" + string(m) }

// HealthPath answers whether the daemon is up. It touches no key material and
// takes no user context.
//
// An answer here also means the daemon verified its PGP root against the
// database before it started listening (#525): a process given the wrong root
// never opens the socket, so this path cannot be reached on one.
const HealthPath = "/healthz"

// MaxRequestBytes bounds a request body. Generous for a key import, tiny by
// HTTP standards: nothing this API accepts is large, and the limit is what
// stops a buggy caller from making the key-holding process allocate.
const MaxRequestBytes = 1 << 20

// MaxSignRequestBytes bounds a sign request, which is the ONE method whose body
// is not a handful of identifiers: it carries the whole message to be signed.
//
// It has to clear the largest message the application will ever hand over. JMAP
// advertises maxSizeUpload = 50 MiB (internal/api), a message may carry several
// such blobs, and JSON encodes []byte as base64 (+33%). 128 MiB is that ceiling
// with room, and is deliberately a SEPARATE constant: raising MaxRequestBytes
// itself would hand the same allowance to import, whose body is a private key
// and has no business being large.
const MaxSignRequestBytes = 128 << 20

// MaxDecryptedBytes is the hard cap on what Decrypt will return: the size of the
// DECOMPRESSED plaintext, not of the ciphertext.
//
// ⚠ UNLIKE SIGN, THE OUTPUT SIZE HERE IS CHOSEN BY THE MESSAGE'S AUTHOR. A
// compressed literal packet can expand by orders of magnitude, so the request
// bound says nothing about the response. go-crypto's per-layer
// MaxDecompressedMessageSize is set from this value, and the reader is wrapped
// in a hard limit on top of it — a message over the cap fails with the same
// opaque code as every other failure.
//
// ⚠ 32 MiB, NOT internal/mime's 64, AND THE REASON IS MEMORY. The crypto
// container runs under a small memory limit (deploy/compose.yml), and one
// decrypt holds the JSON request, the decoded ciphertext, the plaintext and the
// encoded response at once — roughly 5–6× the plaintext at peak. 32 MiB is above
// every mainstream provider's message limit (25 MiB is common), and decrypts are
// serialised (DefaultDecryptConcurrency) so the peak does not multiply. A larger
// encrypted message shows as "could not be decrypted" (ADR-037).
const MaxDecryptedBytes = 32 << 20

// MaxDecryptRequestBytes bounds a decrypt request, which carries one encrypted
// PGP/MIME part: a ciphertext the size of MaxDecryptedBytes, armored (+35%) and
// base64-encoded by JSON (+33%), with room. Its own constant, for the reason
// MaxSignRequestBytes is one: giving that allowance to every method would hand
// it to import, whose body is a private key.
const MaxDecryptRequestBytes = 64 << 20

// MaxDecryptSigners bounds the candidate signer keys one decrypt request may
// carry (RFC 3156 section 6.2, ADR-039 section 6). The application sends the keys
// held for one From address plus the user's own, trimmed by priority to this.
const MaxDecryptSigners = 16

// MaxDecryptSignerBytes bounds the candidate signer keys of one decrypt request
// IN TOTAL (each is also bounded by MaxRecipientKeyBytes). Small on purpose: it
// keeps MaxDecryptRequestBytes's arithmetic true — the largest ciphertext,
// armored and JSON-encoded, plus this, JSON-encoded, still fits
// (TestDecryptRequestBoundCoversSigners pins it).
const MaxDecryptSignerBytes = 1 << 20

// MaxDecryptResponseBytes bounds what the client will read back from decrypt:
// MaxDecryptedBytes base64-encoded by JSON (+33%), with room for the envelope.
const MaxDecryptResponseBytes = 48 << 20

// MaxEncryptInputBytes bounds the MIME entity Encrypt accepts.
//
// ⚠ BELOW MaxDecryptedBytes ON PURPOSE. The Sent copy is the encrypted message,
// and decrypt-on-open refuses plaintext over MaxDecryptedBytes: an entity encrypted
// at exactly that size would be un-openable by its own sender. 24 MiB leaves room
// for the signature wrapper and sits under the 25 MiB message limit most
// providers enforce, so a larger message would be refused at the far end anyway.
const MaxEncryptInputBytes = 24 << 20

// MaxEncryptRecipients bounds how many recipient keys one call carries. Each
// recipient adds a public-key packet to the ciphertext, and a message to more than
// this is a mailing list, not a conversation.
const MaxEncryptRecipients = 100

// MaxRecipientKeyBytes bounds one recipient key on the wire. Matches
// pgpkeys.MaxKeyBytes (a test pins the equality); doubled by the daemon for
// armor.
const MaxRecipientKeyBytes = 256 * 1024

// MaxEncryptRequestBytes bounds an encrypt request: the entity base64-encoded by
// JSON, plus up to MaxEncryptRecipients keys, with room.
const MaxEncryptRequestBytes = 96 << 20

// MaxEncryptResponseBytes bounds what the client reads back: the armored
// ciphertext (+35%) base64-encoded by JSON (+33%) of an entity at the input cap.
const MaxEncryptResponseBytes = 64 << 20

// MaxBytes is the request-body limit for one method. Per-method rather than
// global because the risk differs: an oversized sign request costs memory,
// while an oversized import request means something is wrong with the caller.
func (m Method) MaxBytes() int64 {
	switch m {
	case MethodSign:
		return MaxSignRequestBytes
	case MethodDecrypt:
		return MaxDecryptRequestBytes
	case MethodEncrypt:
		return MaxEncryptRequestBytes
	}
	return MaxRequestBytes
}

// MaxResponseBytes is the response-body limit the client applies for one method.
// Every response except decrypt's is a handful of identifiers or one armored
// key; decrypt's is a whole message.
func (m Method) MaxResponseBytes() int64 {
	switch m {
	case MethodDecrypt:
		return MaxDecryptResponseBytes
	case MethodEncrypt:
		return MaxEncryptResponseBytes
	}
	return MaxRequestBytes
}

// ErrorCode classifies a failure. It is part of the wire contract: callers
// switch on it, and the confused-deputy test asserts on CodeDenied
// specifically, so that a refusal for some unrelated reason cannot be mistaken
// for the boundary holding.
type ErrorCode string

const (
	// CodeInvalidRequest — malformed or missing fields.
	CodeInvalidRequest ErrorCode = "invalid_request"
	// CodeNotFound — no such key, for anyone.
	CodeNotFound ErrorCode = "not_found"
	// CodeDenied — the key exists but does not belong to the calling user.
	//
	// Deliberately distinguishable from CodeNotFound. Hiding existence would
	// buy nothing here (the caller is the application, which is trusted about
	// who it is serving and merely capable of being wrong about which key that
	// user may touch) and would cost the operator a loud, greppable signal for
	// exactly the bug this boundary exists to catch.
	CodeDenied ErrorCode = "denied"
	// CodeInternal — the daemon failed. Never carries key material or the
	// underlying error's detail.
	CodeInternal ErrorCode = "internal"
	// CodeDecryptFailed — the ONE answer decrypt gives for every cryptographic
	// failure: no key of this user's opens it, the integrity check failed, the
	// packets did not parse, the message is not integrity-protected (SED), or
	// the plaintext is over MaxDecryptedBytes. The message is always
	// DecryptFailedMessage, word for word.
	//
	// ⚠ ONE CODE IS THE CONTROL, NOT A SIMPLIFICATION. Distinguishing "wrong
	// key" from "MDC mismatch" from "bad packet" is precisely the oracle that
	// chosen-ciphertext attacks on OpenPGP (EFAIL's family, and the quick-check
	// oracle before it) are built from. TestDecryptFailuresAreIndistinguishable
	// asserts the code AND the message are identical across those cases.
	CodeDecryptFailed ErrorCode = "decrypt_failed"
	// CodeRateLimited — the per-user decrypt budget is spent. Not a
	// cryptographic outcome, so it is allowed to be distinct: it is decided
	// before any key is touched and says nothing about the ciphertext.
	CodeRateLimited ErrorCode = "rate_limited"
	// CodeNoRecipientKey — encrypt was asked to encrypt to a recipient whose key
	// has no valid, unexpired, unrevoked encryption subkey (or cannot be read at
	// all). The message names the ADDRESSES, comma-separated, and nothing about
	// the keys. Its own code so the application can tell the user which
	// recipients to fix without parsing prose.
	CodeNoRecipientKey ErrorCode = "no_recipient_key"
)

// DecryptFailedMessage is the fixed text every CodeDecryptFailed carries.
const DecryptFailedMessage = "the message could not be decrypted"

// Error is a failure returned by the crypto process.
type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("crypto process: %s: %s", e.Code, e.Message)
}

// Errorf builds a wire error.
func Errorf(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrorBody is the JSON envelope every non-2xx response carries.
type ErrorBody struct {
	Error *Error `json:"error"`
}

// GenerateRequest asks for a fresh PGP key pair for UserID.
//
// UserID is the application's assertion of which of its users this call is
// for. The daemon cannot verify it independently — only the application knows
// who is signed in — and does not pretend to. What it does not take on trust
// is which stored key that user may reach: see ExportPublicRequest.
type GenerateRequest struct {
	UserID string `json:"user_id"`
	// Name is the User ID packet's display name. May be empty.
	Name string `json:"name"`
	// Email is the address the key is bound to. Required — a PGP key with no
	// address cannot be published (#48) or matched to a sender.
	Email string `json:"email"`
	// ExpiresDays is how long the key (primary and encryption subkey) stays
	// valid, in days from creation (#795, ADR-040). nil means the default
	// (DefaultKeyLifetimeDays); 0 means never; at most MaxKeyLifetimeDays.
	ExpiresDays *int `json:"expires_days,omitempty"`
	// ExpiresAt is the alternative to ExpiresDays (#813): the exact instant
	// the key (primary and encryption subkey) stops being valid, to the second.
	// The lifetime written to the packets is ExpiresAt minus the key's creation
	// time, so the packets say exactly this moment. At least MinKeyLifetime
	// and at most MaxKeyLifetimeDays from now. Setting both is refused.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// MinKeyLifetime is the shortest validity an exact expiry may ask for (#813):
// one day from now. Short-lived keys are a legitimate request; a key that
// expires before a correspondent's client next refreshes it is not one.
const MinKeyLifetime = 24 * time.Hour

// DefaultKeyLifetimeDays is how long a newly generated key is valid when the
// caller does not say: two years, decided on #795. Expiry is a
// dead man's switch — a key whose holder vanishes stops being encrypted to on
// its own — and pgp-extend renews it without changing the fingerprint.
const DefaultKeyLifetimeDays = 2 * 365

// MaxKeyLifetimeDays caps a lifetime set by generate or extend: ten years. Past
// that "expires" stops meaning anything, and an absurd value is more likely a
// typo (days for seconds) than an intention.
const MaxKeyLifetimeDays = 10 * 365

// KeyInfo is everything about a key that may leave the process. Every field
// here is public by construction; there is no variant of this struct carrying
// private material, and that is the point.
type KeyInfo struct {
	// KeyID is Mailroom's own identifier for the stored key (a ULID), not an
	// OpenPGP key ID. Callers reference keys by this.
	KeyID string `json:"key_id"`
	// UserID is the owner. Echoed back so a caller that mixed up two users
	// sees it in its own logs.
	UserID string `json:"user_id"`
	// Fingerprint is the OpenPGP primary key fingerprint, uppercase hex.
	Fingerprint string `json:"fingerprint"`
	// PublicKey is the ASCII-armored public key — a PUBLIC KEY BLOCK, always.
	PublicKey string    `json:"public_key"`
	CreatedAt time.Time `json:"created_at"`
	// UsableUntil is when the key stops being usable for encryption — the
	// earlier of the primary key's and the encryption subkey's expiry — read off
	// the public key itself. nil means it never expires.
	UsableUntil *time.Time `json:"usable_until,omitempty"`
}

// GenerateResponse is KeyInfo: the caller learns the key exists and gets the
// public half. The private half never leaves the daemon.
type GenerateResponse struct {
	Key KeyInfo `json:"key"`
}

// ExportPublicRequest asks for the public half of a stored key.
//
// This is the shape the confused-deputy test exercises: KeyID is chosen by the
// caller and may be wrong or attacker-influenced, so the daemon resolves it and
// checks the stored owner against UserID before answering.
type ExportPublicRequest struct {
	UserID string `json:"user_id"`
	KeyID  string `json:"key_id"`
}

// ExportPublicResponse carries the armored public key.
type ExportPublicResponse struct {
	Key KeyInfo `json:"key"`
}

// ImportRequest hands an existing PGP key to the crypto process.
//
// ⚠ THIS IS THE ONE REQUEST THAT CARRIES A PRIVATE KEY. Everything else in this
// package is about what must not come OUT; here the material goes IN, and the
// obvious ways to lose it are different:
//
//   - logging the request body, which defeats the whole boundary by the least
//     interesting route imaginable. The daemon never logs bodies, and a test
//     drives an import with the logger captured and asserts the key does not
//     appear in anything it wrote.
//   - echoing it back in the response. ImportResponse is a KeyInfo, which has
//     no field that could carry it, and the leak test calls this method like
//     every other.
//
// The caller should zeroize its own copy once the call returns. It cannot be
// made to, which is why the far more important half is that the key never has
// to travel this way at all: generate is the normal path, and import exists for
// a key that already had a life before Mailroom.
type ImportRequest struct {
	UserID string `json:"user_id"`
	// PrivateKey is an ASCII-armored OpenPGP PRIVATE KEY BLOCK. If it is
	// passphrase-protected, Passphrase must be supplied: the crypto process
	// unlocks it once, at import, and never holds the passphrase afterwards, so
	// signing is never interactive.
	PrivateKey string `json:"private_key"`
	// Passphrase, optional, unlocks a passphrase-protected PrivateKey (the file
	// pgp-export-wrapped writes). The crypto process decrypts the secret packets
	// with it and then stores the key like any other import. ⚠ Like PrivateKey
	// it must never be logged or echoed; it travels only in this request body.
	Passphrase string `json:"passphrase,omitempty"`
}

// ImportResponse describes the stored key. Public fields only.
type ImportResponse struct {
	Key KeyInfo `json:"key"`
	// AlreadyStored reports that this fingerprint was already held, in which
	// case nothing was written. Not an error: importing a key twice is a
	// no-op the caller should be able to say plainly.
	AlreadyStored bool `json:"already_stored"`
}

// BindIdentityRequest makes a key the default signing key for one address.
type BindIdentityRequest struct {
	UserID     string `json:"user_id"`
	KeyID      string `json:"key_id"`
	IdentityID string `json:"identity_id"`
}

// BindIdentityResponse confirms the binding.
type BindIdentityResponse struct {
	Key KeyInfo `json:"key"`
}

// SignRequest asks for a detached signature over Data with a stored key.
//
// Same shape as ExportPublicRequest, and for the same reason: KeyID is the
// caller's choice and may be wrong, so the daemon resolves it and checks the
// stored owner before the key is ever unsealed.
type SignRequest struct {
	UserID string `json:"user_id"`
	KeyID  string `json:"key_id"`
	// Data is signed EXACTLY as given. The crypto process does no
	// canonicalisation of its own, because RFC 3156 §5 canonicalisation is a
	// property of the MIME entity — only the caller knows where the entity
	// starts and ends, and a second, well-meaning normalisation here would
	// change bytes the caller already committed to transmitting.
	Data []byte `json:"data"`
}

// SignResponse carries a signature and nothing else that came out of the key.
//
// ⚠ Every field here is public by construction. A detached signature discloses
// the signer, not the signing key; Fingerprint is already public; MicAlg names
// a hash algorithm. There is no field a private key could arrive in, and the
// leak test calls this method with a real key to keep it that way.
type SignResponse struct {
	// Signature is an ASCII-armored DETACHED signature (PGP SIGNATURE block).
	Signature string `json:"signature"`
	// Fingerprint identifies the key that signed, so the caller can record what
	// it actually got rather than what it asked for.
	Fingerprint string `json:"fingerprint"`
	// MicAlg is the value for RFC 3156's micalg= parameter, e.g. "pgp-sha256".
	//
	// It is REPORTED BY THE SIGNER rather than chosen by the caller because the
	// caller does not decide the hash: go-crypto may override the configured
	// hash to one the key's own preferences accept. A micalg that disagrees with
	// the signature is not a cosmetic defect — some verifiers refuse the message
	// outright, and the failure surfaces at the recipient, where nobody here can
	// see it.
	MicAlg string `json:"micalg"`
}

// MinExportPassphrase is the shortest passphrase export-wrapped will accept.
//
// ⚠ A MINIMUM IS NOT SECURITY THEATRE HERE, because the passphrase is the ONLY
// thing standing between the exported file and the key. Mailroom's own root does
// not protect it — that is the point of the operation — so an export wrapped
// under "x" is, in every way that matters, an export of the raw key. The bar is
// low enough not to obstruct anyone and high enough that the file is not opened
// by inspection.
const MinExportPassphrase = 12

// ExportWrappedRequest asks for a passphrase-encrypted copy of a private key.
//
// ⚠ THIS REQUEST CARRIES A SECRET IN THE OTHER DIRECTION. Like ImportRequest it
// must never be logged: the passphrase is what protects the response, so a
// daemon that logged both would have defeated the operation twice over. The
// daemon does not log bodies, and the import leak test's logging assertion
// covers this path for the same reason.
type ExportWrappedRequest struct {
	UserID string `json:"user_id"`
	KeyID  string `json:"key_id"`
	// Passphrase is chosen by the USER. The process neither stores it nor can
	// re-derive it; losing it means losing the exported key.
	Passphrase string `json:"passphrase"`
}

// ExportWrappedResponse carries the armored, passphrase-encrypted private key.
type ExportWrappedResponse struct {
	Key KeyInfo `json:"key"`
	// PrivateKey is an ASCII-armored OpenPGP key whose secret packets are
	// encrypted under the requested passphrase. Armored because the recipient is
	// a person putting it in a file, and every other OpenPGP tool expects armor
	// at that boundary.
	PrivateKey string `json:"private_key"`
}

// DecryptPurpose says why the application asked, and is recorded in the audit
// row. It does not change what the crypto process does.
type DecryptPurpose string

const (
	// DecryptForBody is Email/get rendering a message the user opened.
	DecryptForBody DecryptPurpose = "body"
	// DecryptForAttachment is a download or preview of one part inside an
	// encrypted message, re-derived by decrypting again (ADR-037).
	DecryptForAttachment DecryptPurpose = "attachment"
)

// DecryptRequest asks for one PGP/MIME encrypted part (RFC 3156 §4, the
// application/octet-stream body part) to be opened with the user's keys.
//
// There is no KeyID. The crypto process tries every key the user holds,
// INCLUDING retired ones — a key retired for signing still decrypts the mail
// that was encrypted to it, forever.
type DecryptRequest struct {
	UserID string `json:"user_id"`
	// MessageID is the Mailroom message the ciphertext came from. It is what
	// the audit row is keyed on — "decrypted N messages for user U" must be
	// reconstructable — and, like UserID, it is the caller's assertion.
	MessageID string         `json:"message_id"`
	Purpose   DecryptPurpose `json:"purpose"`
	// Ciphertext is the OpenPGP message, ASCII-armored (as RFC 3156 transmits
	// it) or binary.
	Ciphertext []byte `json:"ciphertext"`
	// Signers, optional, are candidate PUBLIC keys for a signature embedded in
	// the OpenPGP message itself (RFC 3156 section 6.2, "combined": one-pass
	// signature, literal data and signature packets inside the encryption, as
	// Thunderbird and GnuPG send). The application chooses them for the
	// message's From address and decides what a result MEANS (ADR-039 section 6);
	// the crypto process only says whether one of them made a good signature.
	// They are never used to decrypt. At most MaxDecryptSigners keys and
	// MaxDecryptSignerBytes in total, each binary or armored; a key carrying
	// private material refuses the request.
	Signers []DecryptSigner `json:"signers,omitempty"`
}

// DecryptSigner is one candidate signer key: an OpenPGP PUBLIC key.
type DecryptSigner struct {
	PublicKey []byte `json:"public_key"`
}

// EmbeddedSignatureStatus is the outcome of checking a signature inside the
// OpenPGP message (RFC 3156 section 6.2). A closed set.
type EmbeddedSignatureStatus string

const (
	// EmbeddedVerified: a good signature by one of the request's Signers
	// (SignerFingerprint names which).
	EmbeddedVerified EmbeddedSignatureStatus = "verified"
	// EmbeddedBad: the signature names one of the Signers and does not check
	// out (content altered, expired or revoked key or signature, a refused weak
	// hash, a missing or malformed signature packet). An expired key is told
	// apart by EmbeddedSignature.KeyExpired.
	EmbeddedBad EmbeddedSignatureStatus = "bad"
	// EmbeddedNoKey: signed by a key that is not among the Signers. Says nothing
	// about the signature's validity.
	EmbeddedNoKey EmbeddedSignatureStatus = "noKey"
)

// EmbeddedSignature reports a signature inside the OpenPGP message.
//
// ⚠ NO LIBRARY TEXT. A status and public identifiers only, decided after the
// whole body was read (the signature is checked at EOF; anything earlier is
// meaningless). An unsigned message is {Signed: false} and nothing else.
type EmbeddedSignature struct {
	Signed bool                    `json:"signed"`
	Status EmbeddedSignatureStatus `json:"status,omitempty"`
	// IssuerFingerprint is what the signature says made it, uppercase hex (it
	// may be a signing subkey's). Empty when the signature carries no issuer
	// fingerprint; IssuerKeyID is then the only name.
	IssuerFingerprint string `json:"issuer_fingerprint,omitempty"`
	// IssuerKeyID is the 16-digit uppercase hex key id the signature names.
	IssuerKeyID string `json:"issuer_key_id,omitempty"`
	// SignerFingerprint is the PRIMARY fingerprint of the candidate key that
	// verified it (EmbeddedVerified only), the form the application stores.
	SignerFingerprint string `json:"signer_fingerprint,omitempty"`
	// KeyExpired (EmbeddedBad only, #795/#796): the signature VERIFIED over the
	// content and was refused only because the candidate key that made it has
	// expired. Not altered content, not a forgery; the status stays bad (ADR-039
	// §6) and this is the reason. Never set for a refused weak hash.
	KeyExpired bool `json:"key_expired,omitempty"`
	// ExpiredSignerFingerprint is the PRIMARY fingerprint of that expired key.
	ExpiredSignerFingerprint string `json:"expired_signer_fingerprint,omitempty"`
	// SignedAt is the signature's creation time (KeyExpired only).
	SignedAt *time.Time `json:"signed_at,omitempty"`
	// KeyExpiredAt is when the signing key expired (KeyExpired only; omitted
	// when it cannot be read off the key).
	KeyExpiredAt *time.Time `json:"key_expired_at,omitempty"`
}

// DecryptResponse carries the decrypted MIME entity and the result of checking
// an embedded signature, and nothing else.
//
// ⚠ No session key, no key id, no fingerprint of the decrypting key, and no
// detail about the packets. The fingerprint is recorded in the audit row, which
// is where an operator reconstructs which key was used; the application has no
// use for it, and every field here is one more thing a test must prove clean.
// Signature names only the SIGNER's public identifiers.
type DecryptResponse struct {
	// Entity is the decrypted RFC 3156 §4 plaintext: a complete MIME entity,
	// content headers included.
	Entity []byte `json:"entity"`
	// Signature is the RFC 3156 section 6.2 result. A bad signature does not
	// withhold Entity: the message decrypted, and the reader may read it.
	Signature EmbeddedSignature `json:"signature"`
}

// EncryptRecipient is one To/Cc recipient and the public key to encrypt to.
type EncryptRecipient struct {
	// Address is the recipient's email address. It is used ONLY to name the
	// recipient in a refusal; the daemon does not match it against the key's
	// user ids (a key need not carry one, and the application chose this key).
	Address string `json:"address"`
	// PublicKey is the recipient's OpenPGP PUBLIC key, binary or armored. A key
	// carrying private material is refused.
	PublicKey []byte `json:"public_key"`
}

// EncryptRequest asks for one MIME entity to be encrypted.
type EncryptRequest struct {
	UserID string `json:"user_id"`
	// KeyID is the sender's own key (the identity's bound signing key). Its
	// PUBLIC half is added as a recipient so the Sent copy can be read back; it
	// is ownership-checked like sign's.
	KeyID      string             `json:"key_id"`
	Recipients []EncryptRecipient `json:"recipients"`
	// Entity is the RFC 3156 §4 plaintext: a complete MIME entity, content
	// headers included (normally a multipart/signed).
	Entity []byte `json:"entity"`
}

// EncryptedTo records which key was encrypted to for one address.
type EncryptedTo struct {
	Address     string `json:"address"`
	Fingerprint string `json:"fingerprint"`
}

// EncryptResponse carries the ciphertext. Public by construction: an armored
// OpenPGP message plus the fingerprints of the PUBLIC keys it was encrypted to.
// There is no field a private key or session key could arrive in.
type EncryptResponse struct {
	// Ciphertext is the ASCII-armored OpenPGP message (PGP MESSAGE).
	Ciphertext string `json:"ciphertext"`
	// To lists the key used for each recipient address, in request order, so
	// the caller can record what it actually got.
	To []EncryptedTo `json:"to"`
	// SelfFingerprint is the sender's own key that was added as a recipient.
	SelfFingerprint string `json:"self_fingerprint"`
}

// ExtendRequest asks for a stored key's expiry to be moved to now + Days, or to
// exactly ExpiresAt (#813). Exactly one of the two.
type ExtendRequest struct {
	UserID string `json:"user_id"`
	KeyID  string `json:"key_id"`
	// Days from NOW (not from creation): 1..MaxKeyLifetimeDays. 0 when
	// ExpiresAt is set.
	Days int `json:"days,omitempty"`
	// ExpiresAt is the exact new expiry, to the second: at least
	// MinKeyLifetime and at most MaxKeyLifetimeDays from now.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// ExtendResponse carries the re-signed public key; KeyInfo.UsableUntil is the
// new expiry, read back off the artefact rather than echoed from the request.
type ExtendResponse struct {
	Key KeyInfo `json:"key"`
}

// The reasons a key may be revoked for, mapped to RFC 9580's reason codes.
const (
	RevokeCompromised = "compromised" // key material compromised
	RevokeSuperseded  = "superseded"  // key is superseded
	RevokeRetired     = "retired"     // key is retired and no longer used
)

// RevokeRequest asks for a stored key to be revoked.
type RevokeRequest struct {
	UserID string `json:"user_id"`
	KeyID  string `json:"key_id"`
	// Reason is one of RevokeCompromised, RevokeSuperseded, RevokeRetired.
	Reason string `json:"reason"`
	// Confirm must be the last 8 hex digits of the key's fingerprint
	// (case-insensitive). The daemon checks it against the key it resolved, so a
	// wrong key id is refused, not revoked.
	Confirm string `json:"confirm"`
}

// RevokeResponse carries the revoked public key (still public, and worth
// publishing: a revocation is how correspondents learn to stop).
type RevokeResponse struct {
	Key       KeyInfo   `json:"key"`
	RevokedAt time.Time `json:"revoked_at"`
}
