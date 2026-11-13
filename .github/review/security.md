# Security

Apply the shared rules. Review direction: security.

Does this PR create or expose a security hole? Trace attacker-controlled input
through the handler, shared code and storage. Check relevant changes for:

- Access across instances, object ownership, reserved doctypes, permission
  verbs/selectors, OAuth scopes and sessions. Being logged in is not permission.
- Sharing, revocation, shared-drive inheritance, public links, references and
  changes feeds. Check both senders and recipients.
- File/archive traversal, symlinks, upload/download limits and cleanup. A clean
  path or string prefix alone does not prove a file stays inside its directory.
- SSRF through outbound requests, redirects or DNS; misplaced trust in proxies.
- Query, command or template injection; token handling, cryptographic checks and
  secrets exposed in responses or logs.
- Unbounded work/storage, retries, races and shared resource exhaustion.
- Dependency or CI changes that weaken protections. Use supplied scanner results
  for known vulnerabilities; do not invent CVEs.

For each finding, show the attacker's access, input, code path, failed protection
and consequence. A guard that could be removed later is not a current hole.
Label optional hardening as a suggestion and explain its benefit.
