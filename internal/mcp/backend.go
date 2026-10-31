// SPDX-License-Identifier: AGPL-3.0-only

// Package mcp serves Mailroom's Model Context Protocol tools (#49, spec §13a,
// ADR-044) over the standard Streamable HTTP transport.
//
// IT IS A THIN CLIENT OF THE JMAP API, NOT A SECOND SURFACE (ADR-042
// "Consequences"). Every tool call becomes ONE JMAP request handed to the
// Backend, which runs it through the API's own dispatcher — and therefore
// through the API-key gate: scopes, the account restriction, rate and budget,
// confirm-mode holds, the audit and the anomaly rules. Nothing in this package
// touches the store, reads a key's settings, or decides whether a call is
// allowed. The one policy-shaped question it asks, "may this key use this
// tool?", only HIDES tools from tools/list; it is answered by the Backend from
// the API's own key policy table, and a hidden tool called anyway is refused by
// the dispatcher exactly like any other call.
//
// ANTI-CORRUPTION LAYER (spec §3). This package is the only one that imports
// the MCP SDK (TestOnlyThisPackageImportsTheSDK). Its exported surface is
// plain Go and net/http: NewHandler returns an http.Handler, and the Backend
// speaks JMAP invocations.
package mcp

import (
	"context"
	"encoding/json"
)

// Invocation is one JMAP method call or response, [name, arguments, callId]
// (RFC 8620 §3.2). A response named "error" is a method-level error.
type Invocation struct {
	Name   string
	Args   json.RawMessage
	CallID string
}

// Use is one way a tool uses a JMAP method. tools/list shows a tool only when
// the caller's key may make every one of its uses (Backend.KeyMayCall).
//
// Most methods need fixed scopes, so Method alone says it. Email/set and
// EmailSubmission/set need scopes that depend on what the call does; for them
// a Use also carries Args — representative arguments with the shape the tool
// sends (a create, a destroy) — or, for an Email/set UPDATE, EmailUpdate: the
// fields of the store's EmailUpdate that the tool's patch sets, which the
// API's own field table classifies. Both are classified by the API's key
// policy table, never here.
type Use struct {
	Method      string
	Args        json.RawMessage
	EmailUpdate []string
}

// Backend is what the MCP tools need from the JMAP API.
type Backend interface {
	// Call executes calls as one JMAP request through the API's dispatcher,
	// with ctx carrying the authenticated user and the key's enforced policy,
	// and returns the method responses in order. An error means the request
	// could not be made at all (no key in ctx, too many calls); a refused or
	// failed METHOD is an "error" response, not an error.
	Call(ctx context.Context, calls []Invocation) ([]Invocation, error)

	// KeyMayCall reports whether the key in ctx may make every use, computed
	// from the API's key policy table. Read-only and advisory: it filters
	// tools/list and enforces nothing.
	KeyMayCall(ctx context.Context, uses ...Use) bool

	// KeyLacks is what stops the key in ctx from making the uses: the scopes
	// it lacks (like "mail:send"), or LacksRestriction / LacksUnavailable when
	// no scope would help. Empty when KeyMayCall is true. Advisory, like
	// KeyMayCall: it only words the answer to a hidden tool called anyway.
	KeyLacks(ctx context.Context, uses ...Use) []string

	// SendMode is how the key in ctx sends: not at all, directly, or after the
	// account owner's approval. It only words descriptions and results, so an
	// agent is never told about a send it cannot make.
	SendMode(ctx context.Context) SendMode
}

// What KeyLacks reports when no scope would make a use possible.
const (
	// LacksRestriction: the key is restricted to some accounts and the method
	// cannot keep to that restriction.
	LacksRestriction = "account-restriction"
	// LacksUnavailable: the method is not open to API keys at all.
	LacksUnavailable = "unavailable"
)

// SendMode is how a key sends (Backend.SendMode).
type SendMode int

const (
	// SendNone: the key cannot send; the owner sends its drafts.
	SendNone SendMode = iota
	// SendDirect: send_draft queues the message.
	SendDirect
	// SendWithApproval: send_draft waits for the owner's approval.
	SendWithApproval
)
