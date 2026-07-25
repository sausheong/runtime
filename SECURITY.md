# Security policy

## Supported versions

Runtime is pre-release. Security fixes are made on the current default branch
and included in the next tagged release. Older tags are not currently supported
with backported fixes.

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability. Use GitHub's private
security-advisory reporting for this repository. Include the affected version
or commit, deployment mode, reproduction steps, impact, and any suggested
mitigation.

The maintainer will acknowledge a complete report, assess severity, coordinate
a fix and disclosure date, and credit the reporter when requested. Do not
access data that is not yours, disrupt a shared service, or publish exploit
details before a coordinated fix is available.

## Security boundaries

The authenticated control plane is the tenant boundary. Local agents are
trusted platform processes unless they are placed in separate pods, hosts, or
VMs with restricted credentials and networking. A Docker socket is a host-level
privilege boundary. Transcript redaction is best effort and does not replace
data classification, encryption, retention, or access control.

See [operator-guide.md](operator-guide.md) for deployment hardening and
[RELEASING.md](RELEASING.md) for release integrity.
