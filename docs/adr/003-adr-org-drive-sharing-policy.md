# ADR: Organization drive member sharing policy

## Status

Draft

## Context

Shared drives currently use `Member.ReadOnly` to control whether an individual
member can modify drive content. Organization drives also need drive-wide
limits on how members can create additional shares.

The policy must independently control sharing by public link and sharing by
recipient invitation.

## Decision

Each organization-drive `io.cozy.sharings` document will store an embedded
member sharing policy:

```json
{
  "member_sharing_policy": {
    "link": "none | read_only | read_write",
    "email": "none | read_only | read_write"
  }
}
```

This policy is part of the sharing document, not a separate CouchDB entity.
Each top-level organization drive has its own policy.

A missing policy is equivalent to `read_write` for both channels, preserving
the current behavior. Effective sharing capability is the intersection of
`Member.ReadOnly` and the drive policy:

- `read_write` allows read-write or read-only links and invitations, subject
  to the member's own access.
- `read_only` allows only read-only links or read-only recipient invitations.
- `none` disables that sharing channel for members.

Actions performed directly on the organization instance bypass the member
policy. The policy can be supplied when creating a drive and changed through
`PATCH /sharings/:id`.

Existing recipients keep their access. Existing member-created links are
immediately capped by the current policy without rewriting their permission
documents.

## Consequences

- Existing organization drives require no migration.
- `Member.ReadOnly` and existing sharing-rule semantics remain unchanged.
- Policy resolution stays separate from content-level `CanRead` and
  `CanWrite`.
- Owner-side and delegated-recipient handlers remain authoritative.
- This decision does not introduce item-level email-sharing endpoints or
  nested organization drives.
