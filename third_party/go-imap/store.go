package imap

// StoreOptions contains options for the STORE command.
type StoreOptions struct {
	UnchangedSince uint64 // requires CONDSTORE
}

// StoreFlagsOp is a flag operation: set, add or delete.
type StoreFlagsOp int

const (
	StoreFlagsSet StoreFlagsOp = iota
	StoreFlagsAdd
	StoreFlagsDel
)

// StoreFlags alters message flags.
type StoreFlags struct {
	Op     StoreFlagsOp
	Silent bool
	Flags  []Flag
}

// StoreGmailLabels alters a message's Gmail labels (X-GM-LABELS). Mailroom
// patch, see MAILROOM-PATCHES.md. It requires the X-GM-EXT-1 extension.
//
// Labels are decoded names. A system label keeps its backslash (`\Inbox`) and
// is written as an atom; any other label is a user label, written as a quoted
// string in modified UTF-7, the form the FETCH parser decodes. A user label
// cannot begin with a backslash, because Gmail would read it as a system
// label; the encoder refuses one.
type StoreGmailLabels struct {
	Op     StoreFlagsOp
	Silent bool
	Labels []string
}
