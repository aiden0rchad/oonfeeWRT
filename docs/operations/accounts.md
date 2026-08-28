# Accounts, roles, and sessions

Controller accounts are local to oonfeeWRT. They are separate from router logins and from the runtime passphrase that unlocks `keyring.json` at startup.

> **Outcome:** Each person has an individual account with the least role needed, and owners can review or revoke active sessions without sharing credentials.

## Prerequisites and impact

- The first owner account must already exist.
- Only an owner can create, change, disable, delete, or reset another controller account.
- Sensitive owner actions require the owner's current password again when the last password confirmation is more than five minutes old.

**Write impact:** Account and session administration changes the controller database and audit log only. It does not contact or change a router. The assigned role controls what that person may later ask the controller to do.

## Role matrix

Roles are hierarchical: each higher role includes the lower role's permissions.

| Role | Intended use | Important permissions |
|---|---|---|
| **Read-only** (`viewer` internally) | Read-only monitoring | Read dashboards, devices, clients, topology, radios, policy/site state, General events, and own account/sessions. May request focused polling while viewing a device. |
| **Operator** | Day-to-day transient operations | Read-only access plus acknowledged RF scans, on-air verification, and controller-host speed-test start/cancel. |
| **Administrator** | Device and network management | Operator access plus discovery/inspection, adoption/un-adoption, ACL refresh, optional LLDP, reprobe, desired-state editing, Preview/Apply, policy and client intent, polling settings, and diagnostics bundles. |
| **Owner** | Controller custody | Administrator access plus controller-account administration and encrypted backup/restore, including the post-restore write gate. |

Audit events are more sensitive than ordinary General events. The API applies additional authorization after it knows an event's scope.

## Account rules

### Usernames

A username must:

- contain 1–64 ASCII characters;
- start with a letter or digit;
- use only letters, digits, `.`, `_`, or `-`.

Usernames are ASCII case-insensitive for uniqueness. A soft-deleted username remains reserved and cannot be recreated under different capitalization.

### Passwords

- Minimum: 12 Unicode characters.
- Maximum encoded input: 1024 bytes.
- Passwords are stored as Argon2id verifiers, not plaintext.
- Account passwords are not router passwords and are not the controller runtime passphrase.

Use a unique password for each person. Do not create one shared “admin” account when individual accountability matters.

## Create an account

1. Sign in as an owner.
2. Open **Settings → Accounts**.
3. In **Create account**, enter the username.
4. Choose **Read-only**, **Operator**, **Administrator**, or **Owner**.
5. Enter and repeat a password of at least 12 characters.
6. Select **Create account**.
7. If prompted, enter your current owner password to establish a new five-minute reauthentication window, then run the pending action.

The account mutation and its audit event are committed together. If the mutation fails, it is not presented as an audited success.

## Verify a new account

Use a separate private browser window so you do not disturb the owner session:

1. Sign in as the new user.
2. Confirm the header shows the correct username.
3. Open **Settings → My account** and verify the role.
4. Confirm the navigation and controls match the role matrix. A forbidden API operation remains forbidden even if stale UI state displayed a control.
5. Sign out of the test window.

## Change an account's role

1. Open **Settings → Accounts** as an owner.
2. Find the account and select **Role**.
3. Choose the new role.
4. Review the privilege change, reauthenticate if requested, and confirm.

Role-bearing sessions are checked by the API. Do not rely solely on hiding controls in the browser.

The last enabled owner cannot be demoted. Create and verify another owner first if ownership must move.

## Disable or enable an account

Disabling preserves the account and audit identity while preventing authentication.

1. Open **Settings → Accounts**.
2. Select **Disable** or **Enable** beside the account.
3. Review and confirm after any required reauthentication.

The last enabled owner cannot be disabled. Disabled and deleted accounts deliberately authenticate like an unknown username so the login response does not reveal account existence.

## Reset another account's password

1. Open **Settings → Accounts**.
2. Select **Reset password** for the account.
3. Enter and repeat the new password.
4. Reauthenticate as the owner if requested.
5. Confirm the reset.
6. Give the password to the user through a protected channel and have them change it after sign-in when appropriate.

Password changes revoke that account's sessions so an old session cannot outlive the credential reset.

## Change your own password

1. Open **Settings → My account**.
2. Enter the current password.
3. Enter and repeat the new password.
4. Select **Change password**.

All of your sessions, including the current one, are revoked. Sign in again with the new password. This is intentional: a password change that leaves an existing borrowed session active would not fully remove access.

## Review and revoke sessions

Sessions live only in controller memory; they do not survive a daemon restart.

Current limits:

- idle expiry: 12 hours;
- absolute expiry: 7 days;
- password reauthentication window: 5 minutes.

### Your own sessions

1. Open **Settings → My account**.
2. Review peer address, creation, last use, expiry, and the current-session marker.
3. Select **Revoke** for an unfamiliar or no-longer-needed session.
4. Confirm. Revoking the current session signs this browser out.

### Another user's sessions

1. Open **Settings → Accounts** as an owner.
2. Select **Sessions** for the account.
3. Revoke one session or all sessions.
4. Reauthenticate and confirm when requested.

Session revocation cancels requests associated with that session, including its live WebSocket.

## Delete an account

Deletion is soft: it disables the row, removes its password verifier, retains its username for audit identity, and prevents reuse.

1. Open **Settings → Accounts**.
2. Select **Delete**.
3. Review the username carefully.
4. Reauthenticate and confirm.

The last enabled owner cannot be deleted.

## Authentication protections

- Session cookie: `HttpOnly`, `SameSite=Strict`, and `Secure` when the request is HTTPS or the trusted proxy reports `X-Forwarded-Proto: https`.
- CSRF: mutating authenticated requests require the matching `X-Oonfee-CSRF` token.
- Setup and login: same-origin browser checks protect the unauthenticated endpoints.
- Login throttling: ten failures from one socket peer address within five minutes produce a five-minute lockout.
- Account-specific password confirmation: five failures within five minutes produce a five-minute lockout for that session.
- Unknown-user login spends the same Argon2id work as a known-user failure to reduce username timing disclosure.

The login limiter uses the socket peer, not untrusted `X-Forwarded-For`. When every request arrives from one reverse proxy, failed attempts share that proxy peer for throttling.

## Troubleshooting and recovery

### `recent password confirmation is required`

Complete the **Confirm owner identity** form with the current account password. The resulting step-up lasts five minutes for that session.

### `last_owner`

The operation would disable, demote, or delete the final enabled owner. Create and verify a second owner, then repeat the transfer.

### A username is unavailable after deletion

This is expected. Soft deletion reserves the identity so audit history cannot later refer to a different person with the same username.

### A user is unexpectedly signed out

Check whether their password changed, an owner revoked their sessions, the account was disabled/deleted, the 12-hour idle or seven-day absolute lifetime elapsed, or the controller restarted.

### Login is temporarily locked

Wait for the five-minute window instead of retrying continuously. Verify the correct password in your password manager before the next attempt.

### No owner can sign in

Do not edit the SQLite database by hand; account, secret, schema, and audit invariants are transactional. Restore a verified controller backup containing an enabled owner, or recover through a separately tested controller recovery plan.

## Next steps

- [Back up account and controller state](backups.md)
- [Protect sessions with reverse-proxy TLS](../installation/reverse-proxy.md)
- [Review routine maintenance](maintenance.md)
