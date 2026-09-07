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

**Account activity is logged and the log is tamper-evident.** A multi-region
CloudTrail records management events across the account and write events on
the archive, delivering to a bucket separate from the one it describes —
whatever could destroy the archive cannot quietly erase the record of it. Log
file validation is on, so a delivered log can be proven unaltered. Write
events rather than reads is a deliberate trade: reads are the volume driver
and buy exfiltration detection, writes are what would destroy the one asset
here that cannot be rebuilt.

**Destroying the archive is gated, but the gate is honest about its size.**
Versioning makes an accidental overwrite reversible; it does nothing against a
valid credential used deliberately. So every action that would destroy or
expose the archive — deleting objects or versions, suspending versioning,
rewriting the lifecycle rule, re-enabling ACLs, changing encryption or
ownership, and editing this policy itself — is denied unless the caller
authenticated with MFA, and writes encrypted under a caller-supplied KMS key
are refused outright. The condition is `BoolIfExists`, because
`aws:MultiFactorAuthPresent` is absent rather than false on a call made with
long-term access keys; a plain `Bool` test would have exempted precisely the
stolen-key case it exists to stop.

What this does not do is stop this account's own administrator credential.
`AdministratorAccess` includes `iam:CreateVirtualMFADevice` and
`iam:EnableMFADevice`, so the holder of that key can enrol an MFA device of
their own and satisfy the condition legitimately in about five API calls. The
policy is therefore a genuine control against a careless operator, an
accidental `terraform destroy`, and any credential scoped away from IAM — and
a speed bump, plus an audit trail, against a compromised admin key. It is
recorded here at that value and not a higher one.

The lifecycle rule carries the other half. Overwriting an object is an allowed
`s3:PutObject`, and lifecycle expiry is executed by S3 itself rather than by a
principal, so no bucket policy can intervene in it. Left alone, the rule would
have deleted the original versions ninety days after an overwrite, on the
writer's behalf, without a single denied call. It now retains the five most
recent noncurrent versions unconditionally, which makes a single overwrite —
hostile or a bug in the ingest path — permanently recoverable rather than
recoverable for a quarter.

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
- **An administrator credential can still destroy the archive.** The controls
  above raise cost and guarantee a record; they do not stop the account's own
  admin key, because that key can grant itself the second factor. The two
  controls that would actually hold are Object Lock in COMPLIANCE mode, which
  not even the root user can override, and a copy in a separate AWS account.
  Both are deferred, and the reasoning is deliberate: compliance-mode retention
  cannot be shortened by anyone for any reason, so a bug in the ingest path
  would write objects nobody can delete for the life of the retention, and go
  on being billed for them; and a second account is a second identity boundary
  to run for an archive of public sports data. Cross-region replication was considered and rejected as theatre
  for this threat — it lives in the same account under the same credentials,
  and an attacker deletes the copy along with the original. The honest position
  is that total loss here would be genuinely annoying rather than serious, and
  the estate is sized to that. If that stops being true, the gap is named above
  and the fix is known.
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
- The NHL API dropping historical seasons — today the archive is expensive to
  rebuild; at that point it becomes impossible to rebuild, and the off-account
  copy accepted against in §5 stops being optional.

## 7. Recovery procedures

The security alarms point here, so this section has to answer the question
someone actually has at three in the morning. Each procedure assumes the
previous step failed.

**A root sign-in you cannot account for.** Treat the account as compromised
rather than the login as anomalous. Sign in as root yourself, rotate the root
password, and check the root email address and phone number first — an attacker
who reached root very likely changed them, and changing them back is what makes
every other recovery step possible. Then delete the access keys on the
`funandgames` user, review the IAM users, roles and identity providers that
exist against the ones this repository creates, and only then look at data.

**The archive has lost objects.** Do not write anything to the bucket. Every
object is versioned, the five most recent noncurrent versions of each key are
retained regardless of age, and anything newer than ninety days is retained
outright, so the previous state is almost certainly still there as noncurrent
versions. List versions for an affected key, confirm the timestamps, and copy
the last known good version back over the current one. Establish what happened
before restoring in bulk: the size alarm reports a symptom, and the CloudTrail
record says which principal caused it.

**You need to change the archive's bucket policy and have no MFA.** The policy
denies its own modification without a second factor, which is the point.
The escape hatch is the root user, which does have an MFA device. AWS
guarantees the bucket owner's root principal can call Get, Put and
DeleteBucketPolicy even when the policy explicitly denies root, so a root
session can always remove or replace it. Note the guarantee covers only those
three calls: root is not exempt from the deny on deleting objects, so the
policy has to come off first.

**The MFA device itself is lost.** Sign in at the root console, choose
troubleshoot MFA, and sign in using alternative factors. AWS verifies through
the root email address and an automated call to the registered root phone
number, so both must be current — which is why the account-contact events are
alarmed, and worth confirming on a calendar rather than after an incident.

**Confirming what actually happened.** The trail is multi-region with log file
validation enabled, so `aws cloudtrail validate-logs` will prove whether a
delivered log was altered, and `aws cloudtrail lookup-events` gives ninety days
of management events without needing to read the bucket. Note the standing
limitation from section 5: an attacker holding the admin credential can also
reach the log, so an absence of evidence there is not evidence of absence.
