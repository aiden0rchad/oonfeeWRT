---
title: Permissions and sessions
description: Controller roles, step-up authentication, sessions, and the separate router credential boundary.
---

# Permissions and sessions

oonfeeWRT v0.1.4 has local controller accounts with four enforced roles. These
are not OpenWrt accounts: controller authorization and router access are
separate boundaries.

## Role hierarchy

Higher roles include the permissions of lower roles.

| UI role | Stored value | Intended access |
|---|---|---|
| **Owner** | `owner` | Full controller access, account/session administration, portable backup and restore |
| **Administrator** | `admin` | Configure the controller, adopt/manage devices, edit site intent, Preview/Apply, and generate diagnostics; cannot manage accounts or controller backups/restores |
| **Operator** | `operator` | Read state and run approved operational actions such as RF scans, on-air verification, and controller-host speed tests; cannot edit configuration or accounts |
| **Read-only** | `viewer` | View controller state without configuration/account mutations |

Some sensitive event details require Administrator or Owner even though the
general event stream is readable by lower roles.

## Practical permission matrix

| Task | Read-only | Operator | Administrator | Owner |
|---|:---:|:---:|:---:|:---:|
| View dashboard, devices, clients, topology, radios, and general events | Yes | Yes | Yes | Yes |
| View site desired state and policies | Yes | Yes | Yes | Yes |
| Run controller-host speed test or cancel one | No | Yes | Yes | Yes |
| Run an acknowledged RF scan or on-air verification | No | Yes | Yes | Yes |
| Discover, inspect, adopt, re-probe, rename, or un-adopt a device | No | No | Yes | Yes |
| Download the sanitized compatibility report returned by Inspect | No | No | Yes | Yes |
| Edit WLANs, networks, zones, policies, groups, meshes, uplinks, or overrides | No | No | Yes | Yes |
| Preview and Apply configuration | No | No | Yes | Yes |
| Generate/download diagnostics | No | No | Yes | Yes |
| Install or roll back optional LLDP capability | No | No | Yes | Yes |
| List/manage controller accounts and their sessions | No | No | No | Yes |
| Export or restore portable controller backup | No | No | No | Yes |
| Resume router writes after restore | No | No | No | Yes |

The server enforces this matrix; hiding a control in the UI is not the security
boundary.

Inspect and its compatibility report use the same Administrator-or-Owner,
authenticated, CSRF-protected request. Export is not a second server endpoint:
the browser downloads the allowlisted report already returned in that response.
After download, controller roles no longer govern copies of the JSON file.

## First account and owner protection

The unauthenticated setup endpoint works only while no account record exists.
The first account is always an enabled Owner. Deleting accounts does not reopen
first-run setup because deleted usernames remain reserved.

The controller refuses to disable, demote, or delete the last enabled Owner.
This prevents an apparently valid account edit from leaving the controller
without anyone who can manage accounts or restore operations.

## Account rules

- Usernames are 1–64 ASCII characters.
- A username starts with a letter or digit and may then contain letters,
  digits, `.`, `_`, or `-`.
- Username comparison is case-insensitive; a deleted username remains reserved.
- Passwords are at least 12 Unicode characters and at most 1,024 bytes.
- Passwords use Argon2id hashes. The controller does not impose predictable
  composition rules such as “one symbol and one digit.”

Changing an account's password ends all sessions for that account. Disabling,
deleting, or changing a role also updates/revokes affected sessions rather than
letting old authorization continue.

## Sessions

Sessions exist only in controller memory. A controller restart signs everyone
out, including a restart performed during restore.

| Control | v0.1.4 behavior |
|---|---|
| Idle expiry | 12 hours after last use |
| Absolute expiry | 7 days after creation, even when active |
| Session cookie | `HttpOnly`, `SameSite=Strict`; `Secure` when TLS or `X-Forwarded-Proto: https` is present |
| CSRF defense | Separate same-site token cookie echoed as `X-Oonfee-CSRF` on mutations |
| Session management | Every user can view/revoke their own sessions; Owners can view/revoke another account's sessions |
| Live requests | Revocation closes the account's live channel and cancels protected in-flight requests tied to that session |

The UI displays creation time, last use, expiry, peer address, and which session
is current. Peer addresses come from the socket, not an untrusted
`X-Forwarded-For` header.

## Recent-password confirmation

Sensitive Owner actions require recent password authentication. Sign-in starts
the five-minute recent-authentication window; entering the current password
again refreshes that window when it has expired.

Step-up is required for:

- creating, deleting, enabling/disabling, or changing the role of accounts;
- resetting another account's password;
- revoking another account's session(s);
- starting, cancelling, or downloading a portable backup export;
- uploading and previewing a restore;
- confirming a restore; and
- resuming router writes after restore.

Step-up confirms the controller account. Restore confirmation separately asks
for the destination controller's runtime passphrase and the backup's export
passphrase because those secrets protect different keys.

## Login and credential throttling

Sign-in failures are throttled per socket peer address. Ten failures within five
minutes cause a five-minute lockout for that address. Password work is limited
to two concurrent Argon2id derivations to bound unauthenticated memory use.

Within an authenticated session, repeated current-password failures for
credential confirmation are separately bounded: five failures within five
minutes cause a five-minute delay. Unknown usernames still incur password-hash
work so response timing does not trivially reveal account existence.

## Router credentials are not controller roles

An Administrator or Owner may be allowed to initiate adoption, but the device
administrator credential is still required for the approved SSH transaction.
That credential:

- belongs to the OpenWrt device, not the oonfeeWRT account;
- is used for read-only pre-adoption inspection and/or the explicitly approved
  bootstrap/cleanup transaction;
- is not stored by the controller; and
- does not become the steady-state polling credential.

After adoption, the controller stores its scoped `oonfeewrt` credential sealed
in SQLite. The matching keyring and runtime passphrase are required to open it.
See [Architecture](./architecture.md) and [Safety model](./safety.md).

v0.1.3's effective-WAN observation is background collection, not an interactive
account action. The scoped router credential calls `network.interface dump` and
the pre-existing ACL grant for `/sbin/ip -4 route show table all`. The command
is read-only, and upgrading a router already adopted by v0.1.2 requires neither
an ACL refresh nor a device-administrator credential.

v0.1.4 adds a separate read-only router-clock observation. New adoptions include
`luci.getUnixtime` and the compatibility fallback `luci.getLocaltime` in the
reviewed access payload. Devices adopted before v0.1.4 keep ordinary polling,
Apply, and WAN observation without any access change; only router-clock status
requires an Administrator or Owner to supply the device-administrator credential
and approve the updated payload. The refresh does not set the clock, alter NTP,
or require re-adoption.

## Deployment implications

The v0.1.4 listener is plain HTTP. Cookies are marked `Secure` only when the
request is TLS or the reverse proxy supplies `X-Forwarded-Proto: https`.
Therefore:

- bind directly to loopback for local use;
- use a trusted reverse proxy for remote browser access;
- preserve WebSocket upgrades and `X-Forwarded-Proto`;
- do not expose port 8080 directly to the Internet; and
- do not treat account roles as protection for an untrusted host or stolen data
  directory.

For deployment details, see [Requirements](../reference/requirements.md) and
[`INSTALL.md`](../INSTALL.md).
