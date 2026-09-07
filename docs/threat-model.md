# Threat model

*2026-09-07. Covers HockeyTrack and the
[scoreboard](https://github.com/DavidJDrake/hockeytrack-scoreboard) together,
because they share one AWS account, one event bus and one DNS zone — the
account, not the repository, is the real boundary.*

## Why this document exists

Both repositories are public and both deploy real infrastructure. A reader
should be able to see what was considered, what was decided, and what was
knowingly accepted — not just that a scanner passed.

**A note on what is deliberately not here.** This document describes the
model: assets, boundaries, adversaries, and the controls that exist. It does
not enumerate current unmitigated weaknesses, because publishing those in a
public repository hands an attacker a checklist. Specific open findings are
tracked privately in Jira. That split is itself part of the model: a threat
model is a design artifact and can be public; a findings register is
operational and should not be.

## 1. Assets, in order of what it would actually cost to lose

| Asset | Why it matters | If lost |
|---|---|---|
| **The AWS account** | Everything else lives inside it | Total. Every asset below is downstream of it |
| **The raw archive** | 72,921 games, 11.3 GB, rebuilt over many hours at 3 requests/second against a third-party API | Irreplaceable in practice. The NHL API is the only source and re-fetching is a multi-hour, rate-limited walk |
| **Device private keys** | X.509 keys that authenticate a physical panel | A stolen key can subscribe to public game data. Deliberately worth little — see §4 |
| **The public website** | The project's visible face | Defacement is reputational, not material. Content is regenerated from the archive |
| **The event bus** | The integration point other projects consume | Injected events would render wrong scores on consumers |
| **Notification subscriptions** | Real email addresses of real people | A leak is a privacy issue for third parties, which makes it worse than it looks |

The archive is the crown jewel. Nothing else here is hard to rebuild.

## 2. Trust boundaries

```
   NHL API ──────────▶ poller        untrusted input, third party, no contract
   internet ─────────▶ CloudFront    public read, no write path
   workstation ──────▶ AWS           long-lived admin credentials
   EventBridge bus ──▶ consumers     cross-project, same account
   IoT broker ───────▶ devices       physical devices in other people's homes
   GitHub ───────────▶ world         everything committed is permanent
```

The two that carry the most risk are the least obvious: the developer
workstation, because it holds credentials that can do anything, and GitHub,
because a mistake there cannot be taken back.

## 3. Adversaries actually worth modelling

Not nation states. Realistically:

- **Opportunistic scanners.** Automated, constant, looking for exposed
  buckets, leaked keys in public commits, open endpoints. This is the threat
  that actually arrives, daily.
- **Someone who finds a credential.** In a public commit, a screenshot, a
  pasted log. Historically the most common cause of small-account compromise.
- **A compromised dependency.** The supply chain is the one attack surface
  that scales without anyone targeting you.
- **A person with physical access to a device.** A panel given to family
  lives in a house you do not control.
- **The owner, by accident.** Statistically the most likely source of loss:
  a `terraform destroy` in the wrong directory, a script with a wrong prefix,
  a force-push. This model treats operator error as a first-class adversary
  because it is the one with valid credentials.

## 4. Design decisions that limit blast radius

These are structural — they hold because of how the system is shaped, not
because someone remembers a rule.

**Devices publish nothing.** The IoT policy grants `Connect`, `Subscribe` and
`Receive`, and no `iot:Publish` at all; the device code contains no publish
call, and a test asserts it. So a panel stolen from a relative's living room
cannot inject events, cannot affect any other device, and cannot reach
anything but public game data. Device keys are deliberately low-value.

**Per-device scoping by policy variable.** Subscriptions are pinned to the
connecting thing's own name, so one panel cannot receive another's
configuration even if it tries.

**Replays cannot reach subscribers.** The replay harness publishes under
`hockeytrack.synthetic`; every notification rule pins `hockeytrack.poller`.
A drill is silent by construction rather than by a flag that might default
wrong. Synthetic game ids are the real id plus 9,000,000,000 — eleven digits
where a real id is always ten — and that precondition is enforced in code.

**Least privilege throughout.** Each Lambda has its own role scoped to its
own table, its own log group and its own topics. No wildcard resources in any
policy document.

**The archive is append-mostly and versioned.** Bucket versioning is on,
public access is fully blocked, and objects are encrypted at rest. Nothing in
the pipeline deletes.

**Untrusted input is parsed, never executed.** NHL responses are decoded into
typed structs; raw payloads are stored and forwarded, never evaluated. The
website escapes on output and runs under a CSP with no inline scripts.

**Secrets are absent, not hidden.** No key, certificate or credential exists
in either repository's history — verified across every blob in the object
database, not just the working tree. Configuration carrying real email
addresses is gitignored and required, so a missing file fails the plan
rather than silently destroying subscriptions.

## 5. Accepted risks

Stated so they are decisions rather than oversights.

- **The AWS account number is public.** It appears in Terraform backend
  configuration. It is an identifier, not a credential; it marginally aids
  enumeration. Accepted for the convenience of a working public example.
- **Infrastructure layout is public.** Topic names, table names and IAM
  policies are readable. This is inherent to publishing a worked example, and
  a policy that is only safe while secret is not safe.
- **No logo or third-party assets are redistributed**, which avoids a class
  of licensing risk entirely rather than managing it.
- **The pairing model for the admin site is "you can see the screen".** For a
  device in a living room this is the right threshold; it would not be for
  anything carrying personal data.

## 6. What would change this model

Triggers to revisit, rather than a date:

- A device leaving the owner's household — introduces a person who is trusted
  with hardware but not with the account.
- The admin site gaining accounts — introduces identity, and with it the
  first stored personal data beyond notification emails.
- Any device gaining the ability to publish — currently the single strongest
  structural control, and the fleet-management work would relax it.
- The archive becoming a source others depend on — changes availability from
  a personal inconvenience to an obligation.
