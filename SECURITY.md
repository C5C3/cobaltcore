# Security Policy

CobaltCore (C5C3) is a Kubernetes-native OpenStack distribution for operating
Hosted Control Planes across a multi-cluster topology (Management, Control
Plane, Hypervisor, Storage). This repository delivers everything needed to run
that stack — from infrastructure deployment manifests through the service
operators to the c5c3-operator orchestration layer — built with Operator SDK
(Go), controller-runtime, and Kubebuilder.

We take the security of these components seriously and appreciate the effort of
anyone who reports a potential issue responsibly.

## Supported versions

The project is under active development and has not yet published a tagged
release. Security fixes are applied to the `main` branch, which is the only
branch that currently receives them.

| Version               | Supported          |
| --------------------- | ------------------ |
| `main` (development)  | :white_check_mark: |
| Tagged releases       | none yet           |

Once versioned releases are published, this section will list the release lines
that continue to receive security updates.

## Reporting a vulnerability

Please report vulnerabilities **privately**. Do not open a public issue, pull
request, or discussion for a suspected security problem.

Use GitHub's Private Vulnerability Reporting:

1. Open the repository's **Security** tab.
2. Select **Advisories → Report a vulnerability**.
3. Fill in the report form with as much detail as you can (see below).

Direct link:
<https://github.com/C5C3/forge/security/advisories/new>

A good report includes:

- the affected component (an operator binary, a controller, CRD validation,
  generated manifests/RBAC, or a container image) and, if known, the version,
  commit, or image digest;
- a description of the impact and the conditions required to trigger it;
- reproduction steps, a proof of concept, or the relevant manifests/CR;
- any suggested remediation.

## Scope

**In scope**

- The service operators under `operators/` (Keystone, Glance, Horizon, and any
  later additions) and the c5c3-operator orchestration layer, including their
  controllers and reconcilers.
- CRD schema and validation, including the validating webhooks.
- Generated manifests and RBAC shipped in the Helm charts and `config/`.
- Container images built from `images/`.
- The shared library (`internal/common/`) used by the operators.

**Out of scope**

- Upstream OpenStack services themselves (Keystone, Glance, Horizon, …) — report
  those to the
  [OpenStack Vulnerability Management Team](https://security.openstack.org/).
- Third-party operators and dependencies deployed alongside the stack
  (for example MariaDB, OpenBao, memcached, cert-manager, External Secrets) —
  report those to their respective projects.
- Findings that require privileged cluster access already sufficient to
  compromise the workload by other means.
- User misconfiguration that does not stem from an insecure default shipped by
  this repository.

## Response expectations

- **Acknowledgement:** within 3 business days of the report.
- **Initial assessment:** within 10 business days, including a severity
  estimate and whether the report is accepted, needs more information, or is
  out of scope.
- **Coordinated disclosure:** we aim to release a fix and publish an advisory
  within 90 days of triage. We will keep you informed of progress and agree on
  a disclosure date with you before going public.

Timelines may vary with the complexity of the issue; we will communicate if we
need longer.

## Coordinated disclosure and safe harbor

Good-faith security research is welcome. We will not pursue or support legal
action against researchers who:

- make a good-faith effort to avoid privacy violations, data destruction, and
  service disruption while researching;
- report a vulnerability promptly and give us reasonable time to remediate
  before any public disclosure;
- do not access or modify data beyond what is necessary to demonstrate the
  issue.

If you would like to be credited in the advisory, let us know in your report;
otherwise reports are handled confidentially.
