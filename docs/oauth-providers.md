# Connecting Gmail and Outlook (operator guide)

This guide is for the person who runs a Mailroom server. It shows how to let
the people who use it connect a Gmail / Google Workspace mailbox or an
Outlook.com / Microsoft 365 mailbox by signing in at the provider, instead of
typing an app password. The design is in
[ADR-046](adr/ADR-046-mail-provider-oauth.md) and
[ADR-047](adr/ADR-047-gmail-all-mail-and-labels.md).

Throughout, `https://mail.example.com` stands for the address people use to
reach **your** Mailroom. Replace it with your own.

## 1. What your users get

- In **Settings, Accounts** they see **Connect Gmail** and/or **Connect
  Outlook**. A provider's button appears only when you have set that provider
  up (this guide). If you set up neither, nothing new appears.
- Before leaving for Google or Microsoft, a short page tells them what they
  are agreeing to: Mailroom can read, send, organise and delete all the mail in
  the account, and so can whoever runs the server. They sign in at the
  provider; Mailroom never sees their password.
- If the provider later stops accepting Mailroom's access (the person revoked
  it, changed their Google password, or it sat unused for too long), the
  account shows **needs reconnecting** and a banner appears across the app. Mail
  and changes wait; nothing is lost, and outgoing mail is held, not failed.
  Reconnecting (signing in again, as the same account) picks up where it
  stopped.
- **Disconnect** stops syncing and sending and gives up Mailroom's access. For
  Google, Mailroom also asks Google to remove the access; if that fails the
  person is told to remove it in their Google Account. Microsoft offers no way
  for an app to remove its own access, so people remove Mailroom at their
  Microsoft account's app-permissions page. Either way the mail already in
  Mailroom stays, and the account can be reconnected later (as the same
  provider account only).

**Anyone who can sign in to your Mailroom can connect a mailbox.** Decide
whether that is what you want before you switch this on.

> **Gmail in this release.** Gmail accounts **connect**, but Gmail **sync is
> paused** until a later update: the account is held with sync off, and its
> health says so (code `gmail_sync_pending`). Nothing can switch it on yet, not
> the settings and not `mailroom-admin enable-account`. Changes made in
> Mailroom are also not sent back to Gmail yet.
> Outlook / Microsoft 365 accounts sync straight away. See section 5.

## 2. Set up Google

> Google and Microsoft rename and move their console menus often. If a menu
> name below doesn't match what you see, look for the nearest equivalent:
> the settings themselves (scopes, redirect URI, publishing status, account
> types, permissions) are what matter.

You need a Google account to own the project. It does not have to be a
Workspace account.

1. Open the Google Cloud console (`https://console.cloud.google.com`) and
   create a new project. Name it after your Mailroom.
2. Go to **APIs & Services, OAuth consent screen** (Google's newer console may
   call this "Google Auth Platform"). Choose user type **External**.
3. Fill in the **app name** (your Mailroom's name; people see it), a **user
   support email**, and a **developer contact email**.
4. Under **authorised domains**, add the domain your Mailroom is served from
   (for `https://mail.example.com`, that is `example.com`).
5. Under **scopes**, add `https://mail.google.com/` plus `openid` and `email`.
   `https://mail.google.com/` is the only Google scope that allows IMAP and
   SMTP. Google classes it as **restricted**, which matters for the next step.
6. **Publish the app: set the publishing status to "In production".** Do not
   leave it in "Testing". Google expires the sign-in of a Testing app after 7
   days, so every connected account would need reconnecting weekly.
7. Go to **Credentials, Create credentials, OAuth client ID**. Application
   type **Web application**. Under **Authorised redirect URIs** add exactly:

   ```
   https://mail.example.com/oauth/callback/google
   ```

   It must match to the character: scheme, host, no trailing slash.
8. Copy the **client ID** and **client secret** Google shows you. You will paste
   them into Mailroom's configuration in section 4.

### What your users will see, and Google's limits

You are not going through Google's app verification, which for a restricted
scope is a lengthy process meant for apps published to everyone. So Google
shows people an **"unverified app" warning** before they consent. They choose
**Advanced**, then continue. Mailroom's connect page tells them this is
expected.

Google's policy, as we understand it (check Google's current pages before you
rely on it): an unverified app in production may be used under a "personal
use" allowance, for fewer than 100 users, and Google counts those users over
the **lifetime** of the project; the count cannot be reset. That is plenty for
a household or a small group, and not enough for a public service. Google's API
Services User Data Policy applies even to an unverified app.

If a Google Workspace administrator blocks unverified third-party apps, people
in that Workspace cannot connect this way. See section 7.

### One client per instance

Google allows only 100 refresh tokens per Google account per OAuth client; the
101st silently cancels the oldest. Use a **separate Google OAuth client for each
Mailroom instance** (your test and production servers each get their own), so
two servers never compete for one account's allowance. Separate instances also
need different redirect URIs anyway.

## 3. Set up Microsoft

Outlook.com has stopped accepting passwords over IMAP, so for those users
signing in this way is the only route. You need a Microsoft Entra tenant to own
the app registration (a free one is fine).

1. Open the Microsoft Entra admin center (`https://entra.microsoft.com`), go to
   **Identity, Applications, App registrations**, and choose **New
   registration**.
2. Give it a name (your Mailroom's name). For **Supported account types**
   choose **Accounts in any organizational directory and personal Microsoft
   accounts**. This corresponds to Mailroom's default tenant setting `common`.
3. Under **Redirect URI** choose the platform **Web** and enter exactly:

   ```
   https://mail.example.com/oauth/callback/microsoft
   ```

4. Register it, then copy the **Application (client) ID** from the overview
   page.
5. Go to **API permissions, Add a permission** and add these **delegated**
   permissions (they live under Office 365 Exchange Online / "APIs my
   organization uses" in most portals):
   - `IMAP.AccessAsUser.All`
   - `SMTP.Send`
   - `offline_access`
   - `openid` and `email` (usually added for you)

   Mailroom asks for `https://outlook.office.com/IMAP.AccessAsUser.All` and
   `https://outlook.office.com/SMTP.Send` and refuses the connection if either
   is not granted.
6. Go to **Certificates & secrets, New client secret**. Copy the secret's
   **value** straight away; Microsoft shows it once.

### The client secret expires

A Microsoft client secret has an expiry date you chose when you made it.
**Put that date in your calendar now**, with a reminder a few weeks ahead,
and create a new secret before the old one lapses.

If it does lapse (or the registration is deleted), Microsoft refuses the
Mailroom *server*, not the users. Mailroom does not tell everyone to reconnect;
that would not help. Instead each affected account shows **server sign-in
problem** ("contact the person who runs this server"), the error is logged at
ERROR level for you, and mail and outgoing messages are held rather than
failed. Install a new secret, restart, and the accounts recover without anyone
reconnecting. The same applies to a deleted Google client.

### Things that can stop individual users

- **Work or school accounts** may need their administrator's consent, or the
  organisation may block apps whose publisher is not verified. Publisher
  verification is not practical for a hobby server, so some organisations
  simply cannot be connected.
- **SMTP AUTH may be switched off** for the organisation or mailbox (it is off
  by default under Microsoft's "Security Defaults"). The account then connects
  and **receives** mail, but cannot send. Mailroom says so (code
  `smtp_auth_disabled`), refuses new sends up front, and tells the person to ask
  their administrator to enable Authenticated SMTP for their mailbox, then
  reconnect the account to check again.
- **Personal Outlook.com users must switch IMAP on first**, in Outlook on the
  web: **Settings, Mail, Forwarding and IMAP**. It is off until they do, and
  connecting fails with `mailbox_refused` without it.

## 4. Configure Mailroom

Set these in your deployment's environment. A provider is offered only when
**both** its client ID and secret are set.

| Variable | Meaning |
|---|---|
| `MAILROOM_OAUTH_GOOGLE_CLIENT_ID` | Google client ID. |
| `MAILROOM_OAUTH_GOOGLE_CLIENT_SECRET` | Google client secret. |
| `MAILROOM_OAUTH_MICROSOFT_CLIENT_ID` | Microsoft application (client) ID. |
| `MAILROOM_OAUTH_MICROSOFT_CLIENT_SECRET` | Microsoft client secret value. |
| `MAILROOM_OAUTH_MICROSOFT_TENANT` | Optional. Default `common` (personal accounts and any organisation). `consumers`, `organizations`, a tenant ID or a verified domain narrow it. Match it to the account types you chose in step 2 of section 3. |
| `MAILROOM_PUBLIC_URL` | Optional. The address people reach Mailroom at, e.g. `https://mail.example.com`. Defaults to the origin of `MAILROOM_OIDC_REDIRECT_URL`. Required if you run without OIDC sign-in (dev-auth). |

Every one of these also accepts a `_FILE` form that names a file holding the
value. Use that for the **secrets**, so they stay out of `.env`:

```sh
# .env (not secret)
MAILROOM_OAUTH_GOOGLE_CLIENT_ID=1234567890-abc.apps.googleusercontent.com
MAILROOM_OAUTH_GOOGLE_CLIENT_SECRET_FILE=/run/secrets/mailroom_oauth_google_secret
MAILROOM_OAUTH_MICROSOFT_CLIENT_ID=00000000-0000-0000-0000-000000000000
MAILROOM_OAUTH_MICROSOFT_CLIENT_SECRET_FILE=/run/secrets/mailroom_oauth_microsoft_secret
```

Mount the two secret files into the container with a `secrets:` entry in a
`compose.override.yml` (see [deploy/README.md](../deploy/README.md)). The plain
form works too, for example
`MAILROOM_OAUTH_GOOGLE_CLIENT_SECRET=your-google-client-secret`, but a file is
better.

Then:

1. Restart Mailroom.
2. Read the start-up log. For each enabled provider it prints a line
   "mail-provider sign-in (OAuth) enabled; register this exact redirect URI
   with the provider" with a `redirect_uri` field. Check that value is exactly
   what you registered in sections 2 and 3. If it is not (for example it shows
   a different host), fix `MAILROOM_PUBLIC_URL` and register the printed value.
3. Open Settings, Accounts as a user: the Connect buttons should be there.

**If something is wrong, Mailroom refuses to start and says why**, rather than
quietly offering no button:

- A provider with only one of its ID and secret set: "Google OAuth is
  partially configured: set both ... (or neither)" (likewise for Microsoft).
- A `MAILROOM_OAUTH_MICROSOFT_TENANT` that is not a tenant name, ID or domain.
- A provider configured but no public address to build its callback from: set
  `MAILROOM_PUBLIC_URL`.

If no provider is configured the log says sign-in is off, and that is normal.

## 5. Gmail specifics

**Sync is paused in this release.** A connected Gmail account is held with sync
off. The reason is deliberate: Mailroom's Gmail sync (below) needs a later
update, and syncing a Gmail account without it could download so much that
Google suspends the person's IMAP access everywhere, their phone included. The
server logs a line about the hold at start-up when Google sign-in is on.
Outlook / Microsoft 365 is not affected.

What the Gmail sync will do, once it is switched on:

- **It reads All Mail once**, plus Spam and Trash, rather than every label
  folder, so each message is downloaded once.
- **Gmail labels become Mailroom labels**, and the Inbox, Sent, Drafts, Spam
  and Trash positions are kept. Starred mail arrives as flagged.
- **A daily download budget.** Google limits how much mail an account may
  download through IMAP each day (Google documents 2,500 MB for Workspace
  accounts), and suspends IMAP for the account if it goes over. Mailroom keeps
  to a budget below that. The default is 2,000 MB; change it with
  `MAILROOM_GMAIL_DAILY_DOWNLOAD_MB` (a positive whole number of MB). Raise it
  only if you know your accounts' limit is higher.
- **Newest mail first.** New mail flows within minutes; history is then
  imported backwards a day's budget at a time, so a large mailbox takes days.
  Mailroom tracks progress (messages done out of the total, and an estimate
  of when it will finish) in each account's health report; showing it in
  Settings comes in a later update.
- **A label sweep** re-reads Gmail's labels for already-imported mail every 6
  hours, to catch label changes made elsewhere. Change it with
  `MAILROOM_GMAIL_LABEL_SWEEP_INTERVAL` (for example `6h`; `0` turns it off).
- For very large archives, a Google Takeout import is still the better first
  step.

Outlook and other servers that do not report read/star changes made elsewhere
(no CONDSTORE) use a flag sweep instead: `MAILROOM_FLAG_SWEEP_INTERVAL`
(default `5m`; `0` turns the sweep off), `MAILROOM_FLAG_SWEEP_WINDOW` (default
`500` newest messages per folder) and `MAILROOM_FLAG_SWEEP_FULL_INTERVAL`
(default `6h`, the slower whole-folder pass; `0` turns off only that pass).
The defaults are fine for nearly everyone.

### Previewing a Gmail mailbox: `mailroom-admin gmail-census`

Before sync is switched on for a Gmail account you can see what importing it
would involve, without downloading any message or changing anything:

```sh
mailroom-admin gmail-census --user-id <user ULID> --account-id <account ULID>
```

It reports how many messages and bytes there are, the biggest labels, how many
years the mailbox spans, how many days the import would take at the daily
budget, and a duplicate preview. Options: `--limit N` (sample the newest N per
mailbox; the report says it is a sample), `--top N` (how many labels to list),
`--skip-dedup`, and `--budget-mb N` (try a different daily budget). It also
proves the connection works. It needs the same environment as the server.

## 6. Troubleshooting

After signing in at the provider, the person is returned to Settings with a
message. The codes behind them:

| Code | What it means and what to do |
|---|---|
| `connected`, `reconnected` | Success. |
| `already_connected` | That provider account is already connected for this user. If it stopped working, use **Reconnect** on it. |
| `denied` | The person cancelled at the provider. Nothing was connected. |
| `missing_scope` | They unticked the mail permission on the consent screen. Start again and leave it ticked. |
| `no_refresh_token` | The provider gave no lasting access. Remove Mailroom from the provider account's app permissions, then connect again. |
| `wrong_account` | While reconnecting, they signed in as a different account from the original. Sign in as the original. |
| `mailbox_refused` | The provider signed them in but the mailbox's IMAP refused. Outlook.com: switch IMAP on (section 3). Work or school: ask the administrator to enable IMAP for the mailbox. Gmail: a Workspace administrator may have turned IMAP off. |
| `mailbox_unreachable` | Mailroom could not reach the provider's mail server just now. Try again later. |
| `provider_error` | The provider reported a problem. Try again in a few minutes. |
| `exchange_failed` | Mailroom could not finish the sign-in with the provider. If it keeps failing, check the server log: a wrong client secret or redirect URI is the usual cause. |
| `invalid_state`, `expired`, `provider_mismatch`, `invalid_request` | The sign-in was reused, took more than 10 minutes, or came back mangled. Start again from Settings. |
| `identity_invalid` | The provider did not confirm the account's address. |
| `not_configured` | That provider is not set up on this server (section 4). |
| `server_error` | A fault on the server. Check the log. |

Other things people report:

- **"IMAP access is off for this mailbox ... You do not need to reconnect the
  account."** The sign-in is fine but IMAP is switched off for the mailbox:
  Outlook.com's Forwarding and IMAP setting, or an administrator's policy.
- **"This account can receive mail but not send it."** Microsoft SMTP AUTH is
  disabled for the mailbox (section 3). Once the administrator enables it,
  the person reconnects the account.
- **"needs reconnecting".** The provider no longer accepts the saved access.
  The person uses Reconnect and signs in as the same account.
- **"server sign-in problem".** Not the person's doing. Your client secret has
  expired or the client was deleted or changed. Fix it in the provider console
  and in your configuration (sections 2 to 4).

## 7. Fallback: Gmail app passwords

If a Google Workspace administrator blocks unverified apps, people in that
Workspace cannot use the Google sign-in above. Gmail **app passwords** (which
need 2-Step Verification on the Google account) still work through the ordinary
"add an account" form. Google does not recommend them, and we mention them only
as a fallback. Accounts added this way are ordinary password accounts and are
not subject to the Gmail sync pause described above.
