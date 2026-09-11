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
   workstation ──────▶ AWS           long-lived admin credentials (two users)
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

**Changing the IoT layer pages someone, and devices leave a record.** Both
properties above live in one IoT policy, and a single API call can replace
it. A device cannot make that call. A credential can, so the account-level
security alarms include a rule that fires on every write to the IoT control
plane. The only exceptions are two reads that CloudTrail labels as writes.
The routes the design turned up all pass through such a write:
- a new default policy version, or a policy attached to a certificate
- a certificate bound to a different thing
- a certificate created, registered, transferred in or reactivated
- a CA, certificate provider or provisioning template that would issue
  identities later without any further API call
- a role alias turning a device identity into AWS credentials
- a custom authorizer or domain configuration that skips certificates
- any change to IoT's own logging configuration

Matching every write, rather than listing those events, is deliberate. A list
fails silently if one name is wrong or AWS adds a new API. Matching everything
fails loudly instead. The cost was measured before choosing: in ninety days
the account recorded five other IoT writes, all of them provisioning the first
panel. So provisioning, policy changes, deleting a certificate and detaching a
policy all alert. That is accepted, because the person doing any of them is
the person reading the alert.

On the device side, IoT's own logging is on at ERROR, which records operations
that fail authorization, such as a subscribe the device policy denies. It
writes to the `AWSIotLogsV2` log group, which is kept for ninety days. The
writing is done through a role whose trust policy admits only IoT acting for
this account, pinned by both `aws:SourceAccount` and `aws:SourceArn`. That
role can create that one group and append to it, and nothing else. It is
deliberately denied `PutRetentionPolicy`, so the service cannot reset the
group to never-expire.

Its limits, stated:
- It watches us-east-1, where the policy and certificates live. IoT activity
  in other regions is unwatched. That activity cannot reach the panels, which
  connect to the us-east-1 endpoint.
- It sees the control plane only, not messages published to the panels'
  topics.
- The logging does not record a failed *authentication* at connect, such as a
  revoked or forged certificate. AWS ships that event type
  (`Connection.AuthNError`) disabled by default, as the first deploy showed,
  and the Terraform provider cannot enable it. The CloudWatch metric of the
  same name is emitted regardless, so the scoreboard stack alarms on it, and
  on the other IoT authorization-failure metrics, to the security topic. Each
  alarm sums across the metrics' `Protocol` dimension rather than pinning one
  value: the scoreboard's own refused publishes on 2026-09-07 were tagged MQTT
  although they were made over HTTPS, so a pinned alarm would have missed the
  one real incident.
- Deleting the log group would not stop logging: IoT would recreate the
  group itself, with never-expire retention, and carry on. The history would
  be gone. That deletion, or a shortened retention, now pages through the
  audit log group rule below. Nothing prevents it.
- Removing the logging resource from Terraform leaves logging on, because at
  the pinned provider version its delete makes no API call. Turning logging
  off takes a deliberate call, which the rule catches.
- The role's trust conditions are verified in production, not assumed. On
  the first deploy a deliberately forbidden subscribe, `scoreboard-01` asking
  for `scoreboard/scoreboard-02/config`, was refused. Within two seconds an
  ERROR `Subscribe` entry with reason `AUTHORIZATION_FAILURE` landed in
  `AWSIotLogsV2`, naming the client, its certificate and the topic. So IoT
  can assume the role with both conditions in place.
- Like every alarm here, the rule can be deleted by the administrator
  credential it watches for. Deleting it is itself alarmed. Deleting that
  second alarm is not, which is the second-account argument in §5.

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

**Changing the log groups that hold audit evidence pages someone.** Two
CloudWatch Logs groups are evidence rather than output: the trail's copy,
which the root sign-in alarm reads, and `AWSIotLogsV2`. A rule fires on every
write that names either group. Deleting a group or a stream, shortening
retention, turning off deletion protection, and editing or deleting the root
sign-in filter all pass through such a write. So does adding a subscription
filter, export, KMS key, masking policy or transformer. The hard part is that
the API names a group in several fields, not one: `logGroupName` on older
calls, `logGroupIdentifier` (a name or an ARN) on newer ones, `resourceArn`,
the KMS calls' `resourceIdentifier`, and several list parameters. The list
was taken from every write operation in the CloudWatch Logs service model, and
each field is matched against the name and both ARN forms. Account-wide log
policies name no group but can reach every group, so they alert whatever they
select.

One write is excluded: `CreateLogStream`. It only adds, and every occurrence in
ninety days came from a delivery role. Excluding it costs no detection,
because the only harm an attacker could do with a stream is write forged
events, and that does not need a new stream: `PutLogEvents` writes into an
existing one, and CloudTrail does not record it.

Its limits:
- It watches us-east-1, where both groups live. The trail delivers every
  region into the one group.
- Writes *into* a group are not recorded, so forged entries would not alert.
- Account-scoped resource policies are not matched, because AWS limits them to
  letting services add events. One scoped to either group is matched.
- A future API that names a group through a field not yet in the list would
  not match. That is the one way this rule fails silently rather than loudly.
- The noise measurement covers creation rather than steady state: the trail
  group was four days old and the IoT group under an hour.
- Like every alarm here, the rule can be deleted by the administrator
  credential it watches for. Deleting it is itself alarmed.

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
  admin credentials, because such a key can grant itself the second factor. The two
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
- **Two IAM users hold unrestricted administrator access, not one.** An audit
  for the credential-hygiene work found that `healthtracker-deploy`, created for
  a different project in the same account, also carries `AdministratorAccess`
  and an actively used long-lived key. The account is the trust boundary, so
  that credential can delete this project's archive, its audit trail and its
  alarms, and it sits outside this repository's control entirely. Its own
  project's blast radius is therefore this project's blast radius. Recorded here
  rather than quietly scoped down, because narrowing another project's deploy
  credential without knowing what it needs would trade a security risk for an
  availability one. The fix is to scope it to the resources it actually uses.
- **No MFA device exists on any IAM user.** Only the root account has one.
  A deny conditioned on `aws:MultiFactorAuthPresent` is therefore currently
  unsatisfiable by any principal except root, which is why the enforcement
  policy in `terraform/iam-mfa.tf` ships disabled: enabling it before enrolling
  a device would deny the very calls that enrol one.
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

**An IoT alert you cannot account for.** A device cannot make these calls, so
whoever did holds an AWS credential. Start with the credential: the alert's
Actor line names it, and the procedure above applies. Then put the device side
back:
1. Run a scoreboard `terraform plan`. The provider reads the
   `scoreboard-device` policy's default version, and the logging level and
   role, and shows any difference from the repository. Applying the plan
   creates a new policy version from the repository and sets it as the
   default, and sends the repository's logging settings back.
2. Terraform does not manage certificates or policy attachments;
   `tools/provision.sh` does. So the plan cannot see those. Check what is
   attached to each certificate, and deactivate any certificate you did not
   provision.
3. Delete any authorizer, role alias, CA certificate or provisioning template.
   Neither repository creates any of these, so one that exists is not yours.

**An audit log group alert you cannot account for.** The Actor line names the
credential, and the procedure for a root sign-in applies to it. Then establish
what is left:
1. `aws logs describe-log-groups --region us-east-1 --log-group-name-prefix`
   for `/aws/cloudtrail/hockeytrack-account` and for `AWSIotLogsV2`. Both
   should exist with `retentionInDays` 90 and no `kmsKeyId`, and the trail
   group should report one metric filter, the root sign-in filter.
2. Look for what was added. On each group, check `describe-subscription-filters`,
   `get-data-protection-policy` and `get-transformer`. For the account, check
   `describe-account-policies` for each policy type. Neither repository creates
   any of these, so one that exists is not yours.
3. Run a plan in the repository that owns the group: this one for the trail
   group and its filter, the scoreboard for `AWSIotLogsV2`. The plan compares
   retention with the repository, which is how a group that IoT recreated
   with never-expire retention shows up. Applying it sets retention back.
4. Know what is recoverable. The trail group is a copy: the S3 bucket holds the
   validated original for a year, so nothing it held is lost. `AWSIotLogsV2`
   has no second copy, so deleted IoT history is gone.

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
