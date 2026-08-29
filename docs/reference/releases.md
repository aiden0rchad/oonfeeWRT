# Release notes

The documentation describes the current patch release, **v0.1.3**. Release
artifacts, checksums, container digests, signatures, and attached notes on the
GitHub release are the publication source of truth.

## Current release

- [v0.1.3 release and downloads](https://github.com/aiden0rchad/oonfeeWRT/releases/tag/v0.1.3)
- [v0.1.3 notes in the repository](https://github.com/aiden0rchad/oonfeeWRT/blob/main/RELEASE-NOTES-v0.1.3.md)
- [All GitHub releases](https://github.com/aiden0rchad/oonfeeWRT/releases)

## Earlier releases

- [v0.1.2 notes](https://github.com/aiden0rchad/oonfeeWRT/blob/main/RELEASE-NOTES-v0.1.2.md)
- [v0.1.1 notes](https://github.com/aiden0rchad/oonfeeWRT/blob/main/RELEASE-NOTES-v0.1.1.md)
- [v0.1.0 notes](https://github.com/aiden0rchad/oonfeeWRT/blob/main/RELEASE-NOTES-v0.1.0.md)

Before upgrading, read both the release notes and [Upgrade and roll back](../installation/upgrades.md).

## Verify what you run

For a standalone archive, verify its entry in `SHA256SUMS` before extracting
or installing it. For the OCI image, pin `v0.1.3` or the immutable digest and
verify the GitHub Actions keyless signature as shown in the [Docker Compose
guide](../installation/docker.md).

The macOS binary is not Developer ID signed or notarized. A checksum mismatch
is never an instruction to bypass the check.

## Version and schema boundaries

The daemon prints its build version with:

```sh
oonfeewrtd -version
```

The current documentation targets database schema 19. The controller migrates
supported older state at startup and refuses unsupported downgrades. A rollback
across a schema boundary restores the matching pre-upgrade database and
`keyring.json`; changing only the binary or image tag is not a data rollback.
