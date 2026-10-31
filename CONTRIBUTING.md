# Contributing to Mailroom

Thank you for considering it. Mailroom is a young project with a long roadmap,
and help is welcome in every form: bug reports, testing against providers we
haven't seen, documentation, design feedback and code.

## Ways to help

- **Try it and tell us what broke.** Open an issue with what you did, what you
  expected and what happened. Mail providers differ in ways no test suite
  anticipates, so a report from a provider we don't cover is valuable on its own.
- **Pick up an issue.** Issues labelled
  [`good first issue`](https://github.com/10thTARDIS/mailroom/labels/good%20first%20issue),
  when there are some, are small and self-contained. Anything in the
  [1.x milestone](https://github.com/10thTARDIS/mailroom/milestone/2) is
  current work. Issues labelled `needs-design` need a decision before code;
  commenting with a proposal is the most useful contribution there.
- **Improve the docs.** If something in [`deploy/README.md`](deploy/README.md)
  or [`docs/`](docs/) tripped you up, a fix is a perfect first pull request.
- **Report a security problem privately.** See [SECURITY.md](SECURITY.md);
  please don't open a public issue for a vulnerability.

## Before you start on something big

Comment on the issue (or open one) first, so we can agree the approach. The
spec ([`docs/mailroom-spec.md`](docs/mailroom-spec.md)) is authoritative, and a
change that departs from it needs an ADR in [`docs/adr/`](docs/adr/). That
sounds formal, but an ADR is usually a page: the context, the decision, and what
it rules out.

## Making a change

1. Fork, branch from `main`, and read [docs/development.md](docs/development.md)
   for the layout and the test scripts. Everything runs in Docker.
2. Keep to the house rules: `gofmt` and `golangci-lint` clean, database access
   only through `internal/store`, a `change_log` row for every mutation of a
   JMAP-visible object, and tests for what you change. A test that has never
   failed proves little, so break the code once and watch the test go red.
3. Add a line under `[Unreleased]` in [`CHANGELOG.md`](CHANGELOG.md), written
   for someone using Mailroom rather than for a developer.
4. Open a pull request against `main`. CI runs the full suite; every job must
   pass before merge.

## Privacy in public places

Mailroom handles people's mail, so issues, pull requests, commits and test
fixtures must never contain real mail, real addresses, hostnames or anything
else from a real mailbox or a real deployment. Test data is always synthetic
(`example.com`, `you@example.com`). If a bug only shows up with one real
message, describe its structure or build a synthetic message that reproduces
it. CI runs a portability check that rejects common leaks, but it can't catch
everything.

## Licence

Mailroom is licensed under the
[GNU Affero General Public License v3.0 only](LICENSE). By contributing, you
agree that your contribution is licensed under the same terms. There is no
contributor licence agreement.
