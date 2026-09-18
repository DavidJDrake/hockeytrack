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
| **The device image mirror** | `scoreboard-images-<account>` behind `images.scoreboard.davidjdrake.com`: the `.img.xz` every panel is flashed from and the `latest.json` an unattended panel follows | Code execution on every panel flashed or updated afterwards, on hardware in somebody's home. The objects themselves are rebuildable from a tagged release; what is lost is the assurance that what a panel runs is what was reviewed |
| **The release identity** | `scoreboard-image-publisher`, assumed only by the `image-release` GitHub environment through the shared GitHub OIDC provider | Whoever holds it can publish an image the mirror will serve as genuine. Its trust policy is only as good as the OIDC provider behind it |
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
  credential it watches for. Deleting it is itself alarmed, and so is rewriting
  it. Deleting the rule that alarms on *that* is not, which is the
  second-account argument in §5.

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
CloudTrail carries three selectors: management events across the account plus
write-only data events on the archive's S3 objects; invocations of the three
scoreboard admin-path functions; and row-level data events on the two
scoreboard state tables. It delivers to a bucket separate from the one it
describes — whatever could destroy the archive cannot quietly erase the
record of it. Log file validation is on, so a delivered log can be proven
unaltered. Write-only rather than reads is a deliberate trade scoped to the
archive alone: reads are the volume driver and buy exfiltration detection,
writes are what would destroy the one asset here that cannot be rebuilt. The
Lambda and DynamoDB selectors log both reads and writes.

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
was taken from every write operation in the CloudWatch Logs service model. A
field that can hold only a name matches the exact name, and a field that can
hold an ARN matches the group's ARN as a prefix. Account-wide log policies name
no group but can reach every group, so they alert whatever they select.

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
- ARN fields match by prefix, so a group whose name merely begins with either
  audit group's name would also alert. That can only raise a false alarm, never
  hide a real one. It was accepted because EventBridge rejects a pattern over
  2048 characters, and the exact forms needed 3,942: the first deploy failed on
  it. The Terraform now refuses such a pattern at plan time.
- The noise measurement covers creation rather than steady state: the trail
  group was four days old and the IoT group under an hour.
- Like every alarm here, the rule can be deleted by the administrator
  credential it watches for. Deleting it is itself alarmed, and so is
  rewriting it.

**Changing an alarm pages someone, not only deleting it.** Every control above
is worth exactly as much as the alerting path it runs on, and all of them can be
silenced by a call that rewrites them rather than one that removes them.
Rewriting is also the quieter move: a rule whose pattern no longer matches
anything still exists, still has its target, and still reports as enabled. So a
second rule fires on any write to EventBridge, SNS or CloudWatch that names a
security resource — narrowing a rule's pattern, repointing its target, disabling
it, pushing an alarm's threshold out of reach or emptying its actions, forcing
an alarm to OK, adding a subscription filter policy that drops everything,
rewriting the topic policy so the rules can no longer publish, or adding a
subscriber nobody asked for. As with the IoT and log-group rules, it matches
every such write rather than a list of API names, so a call AWS adds later is
caught rather than missed.

The scoping is a naming convention, which is worth stating plainly because it is
load-bearing: the security rules are `hockeytrack-sec-*`, this stack's security
alarms `hockeytrack-security-*` and the topic `hockeytrack-security-alerts`, so
a single prefix covers all three. Most of the scoreboard's alarms publish to the
same topic, and all of them are named `scoreboard-*`, so that prefix is listed
as well; the scoreboard's own tests keep every alarm there inside it. Two of
them, `scoreboard-iot-publish-retained-auth-error` and `scoreboard-dlq-depth`,
notify the operational topic instead, and rewriting them pages too. Which
request field carries the name differs per call — `name`, `rule`, `alarmName`,
`alarmNames`, `topicArn`, `subscriptionArn`, and both spellings of the tagging
field, which EventBridge and CloudWatch record as `resourceARN` and SNS as
`resourceArn` — and the list came from every write operation in the three
service models, checked against the casing CloudTrail actually records rather
than the casing the models declare.

Building this turned up a latent fault in the companion rule that watches for
deletion: its CloudWatch branch named the wrong event source, so `DeleteAlarms`
and `DisableAlarmActions` could never have matched anything. CloudWatch answers
to two different source values depending on how an event was delivered, and that
rule had the one belonging to the other delivery path. Both are now listed.

Its limits:
- Scoping by name prefix means a renamed resource stops being watched, and the
  scoreboard's alarms are named by the other repository, not this one.
- A call naming one of these resources through a field not in the list would not
  match. This is the same silent failure as the log-group rule, for the same
  reason.
- Rewriting *this* rule is the residual. The event arrives minutes later, by
  which time the pattern it would be matched against is the attacker's own.
  Deleting it is caught, because the deletion rule is not scoped by resource.
- An alarm can also be silenced without calling CloudWatch at all, by stopping
  the data underneath it — a metric filter that no longer matches emits nothing,
  and an alarm with no datapoints is not an alarm that fires.
- The cost was measured before the rule was chosen, while the scoreboard prefix
  was still `scoreboard-iot-`: across ninety days, 33 writes to the three
  services named a security resource, and every one was a `terraform apply` by
  the account's one operator, in this repository or the scoreboard's. Under
  today's `scoreboard-` prefix the same window holds 34, the extra one a write
  to `scoreboard-dlq-depth`. None of the modify-style calls the rule exists for —
  `SetAlarmState`, `DisableRule`, `SetSubscriptionAttributes` and the rest —
  occurred at all, on any resource. So an apply that touches a security
  resource now pages, at least once for every alarm it rewrites, and nothing
  else does. The "up to ten times" once given here was counted under the old
  prefix; the scope is now four of this repository's alarms and all thirteen of
  the scoreboard's. Plans and no-op
  applies stay silent, because Terraform reads these resources rather than
  writing them unless something differs.

**Changing the scoreboard's sign-in gate pages someone.** The scoreboard's
admin site admits only invited Google accounts, and the thing that enforces
that is a Lambda the user pool calls as a trigger, reading the invite list from
one SSM parameter. The pool's own invite-only setting was verified not to stop
Google accounts, so those three resources are an authorization root. A rule
fires on any write that names the pool, the function or the parameter, in
whichever request field names it: dropping the triggers, which fails open to
every Google account; rewriting the function or its configuration; adding an
address to the list; adding an identity provider or an app client; or changing
a user directly. It lists no event names, and it is scoped to the scoreboard's
resources because two other projects run pools, Lambdas and parameters in this
account. The pool is found by name, but only when HockeyTrack itself plans, so
a replaced pool is picked up at HockeyTrack's next apply, not the scoreboard's;
until then the rule watches the old pool ID, and the replacement's own deletion
of that pool is the write that happens to page. Sign-ins themselves do not
page: the per-sign-in Cognito events carry no request parameters to match, and
the gate's own invocations, which the trail now logs, are data events this rule
ignores; a third rule, below, watches those. The scoreboard repository
separately alarms on the gate crashing or being throttled, which are not API
calls. The gate is not the only authorization root, though; the admin API
that consumes the tokens is the other, and the next paragraph covers it.

**Changing the scoreboard's admin API pages someone.** The gate decides who
gets a token; the admin API decides what a token is worth. That decision now
rests on two checks, not one: API Gateway's JWT authorizer runs in front of
both functions as a first gate, and each function also verifies the caller's
ID token itself, against the pool and client named in its own `USER_POOL_ID`
and `APP_CLIENT_ID` environment variables, never trusting the claims the
authorizer hands it in the event. So swapping which issuer the authorizer
trusts no longer claims a panel on its own: a token from a different issuer
still fails the function's own check, is refused with 401, and logs a line
the scoreboard alarms on (`docs/superpowers/specs/2026-09-15-direct-invoke-design.md`
§3.3). What now moves trust is changing a function's own environment —
`UpdateFunctionConfiguration`, which this rule pages on — moving a route or
repointing an integration away from the authorizer, or changing either
function's code or permissions. A second rule fires on
any write that names the `scoreboard-admin` API, in `apiId` or by ARN, or either
function. Like the sign-in rule, it lists no event names and is scoped to the
scoreboard's resources, because the account runs three other HTTP APIs. It
ignores invocations, which the next paragraph covers. What it does not see is
named in its comment, and includes the functions' IAM roles and deletion or
shortened retention of the API's and functions' log groups — both of which
section 13 now pages on — the devices table's ownership rows, which section 14
now pages on, the static site, which section 13 also now pages on, and a
custom domain rerouted away from the API (there is none today). A
scoreboard apply no longer redeploys a function, and so no longer pages,
unless that function's code, dependencies or Go toolchain change: the
scoreboard's build no longer stamps each binary with its commit (the
scoreboard change lands alongside this one), though each binary still
records the Go and module versions that built it.

**Calling the scoreboard's admin functions directly pages someone, and forged
claims are refused.** API Gateway's authorizer checks a token and passes its
claims to the function in the event, but a function cannot tell that event from
one written by hand and sent with `lambda:Invoke`. Anyone in the account allowed
that call could once have acted as any panel owner. Two things close it. The
API and enrollment functions verify the ID token themselves (signature against
the pool's published keys, issuer, exact audience, token use, expiry) and never
read identity from the authorizer's claims; an event whose authorizer block
says a token was accepted but whose token fails logs a line the scoreboard
alarms on. And the trail now logs invocations of those two functions and the
sign-in gate, and a rule pages on any not made by API Gateway or, for the gate,
Cognito. The rule is what sees a genuine token replayed through a direct invoke,
and anything sent to the gate, whose events carry no token. It does not watch
the reducer or the daily schedule function, whose forged invocations would
corrupt displayed game state but grant no control of a panel.

**Changing what the scoreboard's rules lean on pages someone.** Three things
carry the rules above, and none of them was watched. The seven scoreboard
roles, `scoreboard-enroll`'s above all, which may create IoT certificates and
attach the device policy — not all seven belong to a function, but every one
of them was open. The log groups whose metric filters are the refusal, crash
and mismatch alarms, and whose history the recovery procedures read: deleting
a filter silences an alarm without touching it, and shortening retention
destroys the evidence. And the static site's bucket and distribution, which
serve the sign-in page on the real domain. A fourth rule now fires on any
write naming one of them. It is scoped to those resources and lists exactly
one excluded event name, `CreateLogStream`: a 90-day sweep found 1,333 of the
rule's 1,368 pre-exclusion matches were Lambda cold starts creating a stream
and destroying nothing, so that call is excluded by name rather than left to
drown out the other 35. Ordinary site deploys stay silent for a different
reason: an invalidation names the distribution in a different field than a
configuration change does. It does not see objects replaced inside the site
bucket, which are data events the trail does not log; a log stream created to
impersonate a log source, since the exclusion is by name rather than by
origin; nor the DNS record that points the domain at the distribution.

**Reading or changing the scoreboard's rows pages someone.** Every rule above
watches the control plane around two tables; these are the tables. One holds
which account owns which panel, so a single write hands somebody a panel with
no API call and no token. The other holds the hashes of the collection tokens
and claim codes that turn a fresh panel into a device with a certificate, so a
read is enough to matter. The trail now logs both tables' rows, reads
included, and a fifth rule pages on any access that did not come from the
`scoreboard-api` or `scoreboard-enroll` role. Nothing else touches them today,
so the expected noise is none — including the owner's own scan during a
recovery, which pages by design. What it cannot see is those two roles' own
access: a compromised function reads and writes exactly as it should, which is
why widening either role pages separately.

**Changing what a panel runs pages someone, with one deliberate exemption.**
Every rule above protects data or access. This one protects code. The
scoreboard's release workflow builds a Raspberry Pi image, publishes it to
`scoreboard-images-<account>` behind `images.scoreboard.davidjdrake.com`, and
every panel flashed or updated afterwards executes it — so an object replaced
there is not a data loss, it is somebody else's code running on hardware in a
living room. Four things carry that chain and each substitutes an image on its
own: the objects, the distribution that is the only name the panels know, the
`scoreboard-image-publisher` role, and the GitHub OIDC provider its trust
rests on. The trail now logs object writes in the mirror — write-only, because
what it serves is public by design — and a sixth rule pages on any of them
whose session was not issued by the publisher role, and on any write naming
the bucket, the distribution, that role by exact name, or the provider by ARN.
Measured over the ninety days to 2026-09-17: fifteen management writes across
all four, every one of them the single Terraform apply that created them, and
no object writes at all.

Four things it does not see, stated plainly because the first is the largest
residual in this file:

- **The publisher role's own writes are exempt by design.** They are the
  release doing its job, several times per release, and a rule that pages on
  every release is a rule nobody reads. So a stolen publisher session can
  overwrite an older `images/<v>/` object — one nothing is about to
  re-publish, so nothing disagrees about it — and this rule stays quiet. What
  guards a malicious release through the real workflow is the environment
  approval on `image-release` and the build attestation the workflow
  publishes, not detection. The scoreboard's spec §9.12 records the same gap
  from the other side.
- **Invalidations do not page, by anyone.** `CreateInvalidation` names its
  target in `distributionId`; configuration changes name it in `id`, and only
  `id` and the ARN forms are matched. 487 invalidations in ninety days across
  this account made that the right trade, and the reasoning is the
  scoreboard's spec §9.8: an invalidation only makes CloudFront re-read an
  origin whose every write already pages here.
- **The daily monitor is up to 24 hours late.** `scoreboard-imagecheck`
  compares the mirror against the GitHub release; it is what finds the two
  disagreeing after a write this rule exempted, and it runs once a day.
- **GitHub-side tampering is outside AWS entirely.** A GitHub organization
  admin can replace the release asset the workflow uploaded without a single
  AWS API call. The monitor sees that only as the mirror and the release
  disagreeing, and only while the mirror still holds the original.

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
`iam:EnableMFADevice`, so the holder of that key can enroll an MFA device of
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
  a device would deny the very calls that enroll one.
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

**An alerting-path alert you cannot account for.** Someone has rewritten part of
the alarming rather than removed it, so read the alert you are holding as
possibly the last one that will arrive. The Actor line names the credential and
the root sign-in procedure applies to it. Then establish what still works:
1. `aws events describe-rule` and `aws events list-targets-by-rule` for each
   `hockeytrack-sec-*` rule. Compare the pattern and the target's input
   transformer against this repository. A rule can be `ENABLED`, have its
   target, and match nothing.
2. `aws sns get-topic-attributes` for `hockeytrack-security-alerts` and
   `list-subscriptions-by-topic`. The policy should still let the security rules
   publish; there should be no subscription you did not create; and
   `get-subscription-attributes` on each should show no `FilterPolicy`.
3. `aws cloudwatch describe-alarms` for the `hockeytrack-security` and
   `scoreboard` prefixes. Check `ActionsEnabled`, the threshold, and that
   `AlarmActions` still names the security topic.
4. Run a plan in this repository, and one in the scoreboard for its alarms.
   Anything rewritten shows as a difference, and applying puts the repository's
   version back. What a plan cannot show you is an alarm forced to `OK` by
   `SetAlarmState`, because that is state rather than configuration; step 3 is
   what catches it.

**A scoreboard sign-in alert you cannot account for.** Assume someone can admit
an account of their choosing to the scoreboard admin site, and with it claim
panels. The Actor line names the credential, and the root sign-in procedure
applies to it. Then, in us-east-1:
1. `aws cognito-idp describe-user-pool --user-pool-id <id> --query
   'UserPool.LambdaConfig'`. Both `PreSignUp` and `PreTokenGenerationConfig`
   must name `scoreboard-authgate`. If they are missing, the gate is open. No
   other trigger may be set, because the scoreboard configures none; a
   `PreTokenGeneration` key, if Cognito reports one, must name the same
   function.
2. `list-user-pool-clients` must show exactly one client, and
   `list-identity-providers` exactly one provider, `Google`.
3. `list-users`: every user should be an invited address, and none should be
   anything but `EXTERNAL_PROVIDER`.
4. Check which panels a user you did not invite claimed, before removing
   anyone: `aws dynamodb scan --table-name scoreboard-devices
   --projection-expression 'thingName, #o' --expression-attribute-names
   '{"#o":"owner"}'`. Every `owner` must be the `sub` of a user you invited,
   which `list-users` shows. Record any other: that panel is under someone
   else's control.
5. Read the invite list and remove anyone you did not invite
   (`aws ssm get-parameter --name /scoreboard/allowed-emails`, then
   `put-parameter --overwrite`). Do this before step 6, or a deleted account can
   sign straight back in.
6. For each user who should not be there, `admin-user-global-sign-out` and
   then `admin-delete-user`. Neither call ends their access at once: access and
   ID tokens already issued stay valid for up to an hour, because the API's JWT
   authorizer checks a token's signature, issuer, audience and expiry, not
   whether Cognito has revoked it.
7. `aws lambda get-function --function-name scoreboard-authgate` and
   `get-function-configuration`. `ALLOWLIST_PARAMETER` must be
   `/scoreboard/allowed-emails`. The code must be what the scoreboard
   repository builds: at the commit last applied, with the Go version that
   built it (`go version -m` on the `bootstrap` inside the zip that
   `get-function`'s `Code.Location` downloads), run `make build` and then `terraform plan` there, which
   regenerates `build/authgate.zip`. Then compare `aws lambda
   get-function-configuration --function-name scoreboard-authgate --query
   CodeSha256 --output text` with `openssl dgst -sha256 -binary
   build/authgate.zip | base64`; they must be equal. Hash only the zip the plan
   regenerated: the one `make build` writes first is not reproducible and is
   not what Terraform deploys. A plan showing no change is not this check;
   it is step 9's drift check.
8. The admin API, which a separate rule watches. Check it anyway: find the
   `scoreboard-admin` API with `aws apigatewayv2 get-apis`, then
   `get-authorizers --api-id <id>`: its one JWT authorizer's issuer must be
   `https://cognito-idp.us-east-1.amazonaws.com/<pool id>` and its audience the
   site client's ID alone, as the scoreboard repository's `terraform/admin.tf`
   sets them. A different issuer here is not enough on its own: the functions
   verify the token themselves against their own `USER_POOL_ID` and
   `APP_CLIENT_ID`, which the admin API entry's step 4 checks. A different
   authorizer issuer together with a repointed `USER_POOL_ID` or
   `APP_CLIENT_ID` is what would let whoever controls that issuer mint tokens
   the API believes.
9. Run a plan in the scoreboard repository: anything rewritten shows as a
   difference, and applying puts it back. The invite list's value is the
   exception, because Terraform deliberately ignores it, which is why step 5
   reads it by hand.

**A scoreboard admin API alert you cannot account for.** Assume someone can make
the admin API believe a caller it should not, and with it claim or control
panels. The Actor line names the credential, and the root sign-in procedure
applies to it. Then, in us-east-1:
1. `aws apigatewayv2 get-apis` to find `scoreboard-admin`'s ID, then
   `get-authorizers --api-id <id>`. There must be exactly one JWT authorizer,
   whose issuer is `https://cognito-idp.us-east-1.amazonaws.com/<pool id>` and
   whose audience is the site client's ID alone, as the scoreboard repository's
   `terraform/admin.tf` sets them.
2. `get-routes --api-id <id>`. `GET /api/devices`, `PUT /api/devices/{thing}/game`,
   `PATCH /api/devices/{thing}`, `DELETE /api/devices/{thing}` and `GET
   /api/games` must target the api integration and that authorizer. `POST
   /api/enroll` and `GET /api/enroll` must target the enroll integration and be
   unauthenticated by design; `POST /api/devices/claim` must target the enroll
   integration but use that same authorizer (`terraform/admin.tf`
   `local.admin_routes`, `terraform/enroll.tf`). No route may exist that the
   scoreboard repository does not define.
3. `get-integrations --api-id <id>`. Both integrations must be `AWS_PROXY`,
   and each `IntegrationUri` API Gateway's invoke form for a function. The api
   integration's must be
   `arn:aws:apigateway:us-east-1:lambda:path/2015-03-31/functions/arn:aws:lambda:us-east-1:<account>:function:scoreboard-api/invocations`,
   and the enroll integration's the same with `scoreboard-enroll`. Nothing may
   sit between the function name and `/invocations`: a `:<alias>` or
   `:<version>` qualifier there sends traffic to code this check does not see.
4. For each function, run `aws lambda get-function`, `get-function-configuration`,
   `get-policy`, `list-function-url-configs`, `list-event-source-mappings`,
   `list-aliases` and `list-versions-by-function`:
   - the code must be what the scoreboard repository builds. At the commit
     last applied, with the Go version that built it (found as in the
     sign-in entry's step 7), run `make build` and
     then `terraform plan` there, which regenerates `build/api.zip` and
     `build/enroll.zip`. Compare `aws lambda get-function-configuration
     --function-name scoreboard-api --query CodeSha256 --output text` with
     `openssl dgst -sha256 -binary build/api.zip | base64`, and the same for
     `scoreboard-enroll` against `build/enroll.zip`; each pair must be equal.
     Hash only the zips the plan regenerated: the ones `make build` writes
     first are not reproducible and are not what Terraform deploys. A plan
     showing no change is not this check; it is step 9's drift check;
   - the role must be the scoreboard's own;
   - the resource policy must allow only API Gateway, from this API's
     execution ARN;
   - there must be no function URL and no event source mapping;
   - there must be no alias and no version beyond `$LATEST`;
   - each function's `USER_POOL_ID` and `APP_CLIENT_ID` environment variables
     (`aws lambda get-function-configuration --function-name <fn> --query
     Environment.Variables`) must equal the pool ID and the site client ID
     found in step 1. The authorizer is only a first gate; each function
     believes tokens issued by whichever pool and client its own environment
     names, whatever the authorizer's issuer says.
5. For each role, `scoreboard-api` and `scoreboard-enroll`: first `aws iam
   get-role --role-name <role>`. Its trust policy must allow `sts:AssumeRole`
   to the `lambda.amazonaws.com` service alone, and it must have no
   `PermissionsBoundary`, because the scoreboard sets none (`terraform/iam.tf`
   `lambda_trust`, `terraform/enroll.tf`). Any other principal in the trust
   policy can take the role's permissions without going near the function: on
   `scoreboard-enroll` that means minting device certificates with no API
   involved, and the `UpdateAssumeRolePolicy` that would allow it is a call
   HockeyTrack's identity rule deliberately excludes. Then `list-role-policies
   --role-name <role>` and `get-role-policy` for each result, then
   `list-attached-role-policies`. Compare every statement against
   `terraform/admin.tf` and `terraform/enroll.tf`. A credential that can rewrite
   a function's configuration can likely rewrite its role too, and a widened
   grant here — especially on `scoreboard-enroll`, which can mint device
   certificates — is a route this rule does not watch.
6. Check which panels changed hands, as in the sign-in entry's step 4, by
   scanning `scoreboard-devices` for owners who are not invited users. This
   cannot reveal actions taken under a forged invited user's `sub`: an issuer
   the attacker runs can mint a token carrying any invited user's `sub`, so
   ownership rows written that way look legitimate. Steps 7 and 8 are how
   those are found, with a limit: a direct invocation of either function,
   carrying forged claims in a hand-built event, never passes through API
   Gateway, so it leaves no access-log entry. It does leave a CloudTrail
   record now: the trail logs invocations of these functions
   (`terraform/cloudtrail.tf`), and section 12 pages on any not made by API
   Gateway, whether the event carried forged claims or a genuine token. The
   invocation count at the end of step 7 is the check that can show one
   section 12 somehow missed.
7. Read the admin API's access log for the window between the change and the
   fix: `aws logs filter-log-events --log-group-name
   /aws/apigateway/scoreboard-admin --start-time <ms>` (30-day retention).
   Each entry carries `requestId`, `ip`, `route`, `status`, `sub` and `error`
   (`terraform/admin.tf` `access_log_settings`). `route` is the route
   template, such as `PATCH /api/devices/{thing}`, so the log shows which
   routes were called, from where and under which `sub`, but not which panel;
   `scoreboard-devices` shows each panel's current owner, name and game. Look
   for requests from unfamiliar IPs, successful `POST /api/devices/claim`
   calls, `PUT`/`PATCH`/`DELETE` calls the owner did not make, and `GET
   /api/enroll` answered `200`, above all from an unfamiliar IP: that is the
   call that hands a claimed panel's certificate to whoever holds its
   collection token, while a waiting panel's polls are answered `202`.

   Then count invocations, which the functions log whether or not API Gateway
   sent them:
   `aws logs filter-log-events --log-group-name /aws/lambda/scoreboard-api --start-time <ms> --filter-pattern '"START RequestId"'`,
   and the same for `/aws/lambda/scoreboard-enroll` (both 30-day retention).
   Each line is one invocation. Compare them with the access-log entries for
   the routes on that function over the same window. The comparison is by count
   and time, not by ID, because the access log does not record Lambda's request
   ID; and a request refused before the function — an authorizer 401 or a
   throttled 429 — appears in the access log with no `START` line, so expect
   fewer invocations than log entries (on 2026-09-15, 4 entries against 1). More
   invocations than the routes can explain, or an invocation with no API request
   near it, is a direct invoke. This depends on the roles' logs permissions, which step 5 checked,
   and on the log groups still being there with their retention intact, which
   no rule watches.
8. `aws iot list-certificates`, which gives each certificate's `creationDate`
   (`list-things` gives none), and pick those created in the window. For each,
   `aws iot list-principal-things --principal <certificate arn>`. Panel things
   are named `scoreboard-<suffix>` (`cloud/internal/enroll/enroll.go`). For a
   certificate or thing that is not a known panel, in this order: `aws iot
   update-certificate --certificate-id <id> --new-status REVOKED`;
   `detach-policy --policy-name scoreboard-device --target <certificate arn>`,
   and the same for any other policy `list-attached-policies --target
   <certificate arn>` shows; `detach-thing-principal --thing-name <thing>
   --principal <certificate arn>` (asynchronous: retry the next deletes if they
   say the certificate or thing is still attached); `delete-certificate --certificate-id <id>`;
   `delete-thing --thing-name <thing>`; and finally remove the thing's
   ownership row with `aws dynamodb delete-item --table-name
   scoreboard-devices --key '{"thingName":{"S":"<thing>"}}'`.
9. Run a plan in the scoreboard repository. Anything rewritten shows as a
   difference, and applying puts it back.

**A scoreboard direct-invoke alert you cannot account for.** Assume someone
holds a credential in this account and has called a scoreboard admin function
with an event they wrote. In us-east-1:
1. Find the full record. The alert gives the time and the Actor ARN, but the
   log group stamps each record when CloudTrail delivers it, not when the
   call happened, so match on the record's own `eventTime` rather than
   searching close around the alert's time. About five minutes of delivery
   delay was measured (design spec §8: an `eventTime` of 19:36:46, delivered
   at 19:41:53), but CloudTrail documents no upper bound on delivery time, so
   treat that as a starting point, not a guarantee. Start one minute before
   the alert's time and end at least twenty minutes after it, or omit
   `--end-time` if the alert is recent, and widen the window further if
   nothing returns:
   `aws logs filter-log-events --log-group-name /aws/cloudtrail/hockeytrack-account --start-time <ms> --end-time <ms> --filter-pattern '{ ($.eventSource = "lambda.amazonaws.com") && ($.eventCategory = "Data") && ($.userIdentity.type != "AWSService") }'`.
   Note `userIdentity` (its `arn`, `accessKeyId`, and for a role
   `sessionContext.sessionIssuer`), `sourceIPAddress`, which function, and the
   exact `eventTime`. If the alert's Actor line is empty, the caller was an AWS
   service: drop the `userIdentity.type` clause and look instead for the
   record whose `invokedBy` is not `apigateway.amazonaws.com` (or, for
   `scoreboard-authgate`, not `cognito-idp.amazonaws.com`).
2. Cut the credential off before investigating further. For an IAM user's
   long-term key, `aws iam update-access-key --user-name <user> --access-key-id <id> --status Inactive`.
   A key ID beginning `ASIA` is temporary (a session token, not a registered
   access key), and `update-access-key` does not accept it and cannot
   deactivate it; for one of those, attach an inline deny-all policy to the
   user instead:
   `aws iam put-user-policy --user-name <user> --policy-name deny-all-incident --policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"*","Resource":"*"}]}'`,
   and remove it once the credentials are rotated. For a role
   (`userIdentity.type` `AssumedRole`), revoke its active sessions (IAM
   console, the role, Revoke active sessions). For a federated session
   (`FederatedUser`), revoke it at the identity provider or role that issued
   it, the same way. For `userIdentity.type` `Root`, this is a root
   compromise: stop here and run the root sign-in procedure above in full,
   starting with the root password and the contact email and phone. For
   anything else, follow the root sign-in procedure's credential steps for
   whoever owns it. If the credential you just cut off is the one you
   normally use, continue this procedure from a separate one: sign in to the
   root console and use CloudShell.
3. Read what the function did, from one minute before to five minutes after:
   `aws logs filter-log-events --log-group-name /aws/lambda/<function> --start-time <ms> --end-time <ms>`.
   - For `scoreboard-api` or `scoreboard-enroll`, a
     `token rejected after authorizer accepted` line at the invoke's own
     `eventTime` means this call's token failed the function's own
     verification, and `scoreboard-token-mismatch` will have paged as well.
     The same line also fires on two real paths that are not this direct
     invoke — an access token sent through API Gateway by a signed-in user,
     or a Cognito signing-key rotation inside the verifier's refetch window
     — so match it to the invoke by time and route rather than treating any
     mismatch line in the window as proof; a mismatch page with no section
     12 page at the same time is one of those real-path cases, not a direct
     invoke.
   - No such line, and no error, means the event may have carried a genuine
     token and been served. The logs do not record whose. Treat this branch
     as conservative rather than conclusive: it also covers a forged event
     with no authorizer block, which fails the function's own verification
     and is refused, but logs nothing at all, because `idtoken.Authenticate`
     only writes the mismatch line when an authorizer block is present. That
     refusal is indistinguishable in the function log from a served genuine
     token, so treat "no mismatch line" as "assume served" and run the
     recovery either way. Run the admin API entry's panel check (step 6) and
     certificate check (step 8), and sign every user out:
     `aws cognito-idp list-users --user-pool-id <pool id>`, then
     `aws cognito-idp admin-user-global-sign-out --user-pool-id <pool id> --username <username>`
     for each. That revokes refresh tokens; ID tokens already issued stay valid
     until they expire, at most an hour.
   - For `scoreboard-authgate`, a direct invoke does get its caller something:
     the gate's answer tells whoever sent it whether the address in the event
     is on the invite list, so a direct invoke can enumerate invitations. Only
     a refusal writes a log line (`sign-in refused`); an admitted address
     writes none, so a decision in this window with no refusal line means that
     address was admitted. Check its log for the decision, then run the
     sign-in entry anyway, because a credential that can invoke the gate can
     usually change it.
4. Run the admin API entry in full for `scoreboard-api` or `scoreboard-enroll`,
   and the sign-in entry for `scoreboard-authgate`.

**A scoreboard support alert you cannot account for.** Assume one of the
things the scoreboard's other rules depend on has been changed. The alert's
event name says which. In us-east-1:
1. **A role** (`PutRolePolicy`, `AttachRolePolicy`, `UpdateAssumeRolePolicy`,
   `DeleteRolePolicy`, …). List what it holds now:
   `aws iam list-role-policies --role-name <role>`,
   `aws iam get-role-policy --role-name <role> --policy-name <name>`,
   `aws iam list-attached-role-policies --role-name <role>`, and
   `aws iam get-role --role-name <role> --query Role.AssumeRolePolicyDocument`.
   Compare each with the scoreboard repository's `terraform/` (`iam.tf`,
   `admin.tf`, `enroll.tf`, `signin.tf`, `scheduler.tf`, `iot-logging.tf`).
   For `scoreboard-enroll`, also run the admin API entry's certificate check:
   a widened role's whole point is minting device identities.
2. **A log group** (`PutRetentionPolicy`, `DeleteMetricFilter`,
   `PutMetricFilter`, `DeleteLogGroup`, `PutSubscriptionFilter`). Check what
   survives: `aws logs describe-log-groups --log-group-name-prefix /aws/lambda/scoreboard`
   for retention, and `aws logs describe-metric-filters --log-group-name <group>`
   against the scoreboard repository (`signin.tf` and `admin.tf` hold them all).
   A filter that is gone means its alarm has been sitting in
   INSUFFICIENT_DATA rather than firing:
   `aws cloudwatch describe-alarms --alarm-name-prefix scoreboard- --query "MetricAlarms[].[AlarmName,StateValue,StateUpdatedTimestamp]"`.
   A subscription filter that you did not create is exfiltration of the logs;
   delete it. Re-apply the scoreboard repository to restore filters and
   retention.
3. **The site** (`PutBucketPolicy`, `PutBucketPublicAccessBlock`,
   `UpdateDistribution`, `DeleteDistribution`, …). Compare
   `aws s3api get-bucket-policy --bucket scoreboard-site-<account id>` and
   `aws s3api get-public-access-block --bucket scoreboard-site-<account id>`
   with the scoreboard repository's `terraform/site.tf`, and
   `aws cloudfront get-distribution-config --id <id>` for its origins, origin
   access control, cache behaviors, aliases and custom error responses. Then
   check what is being served, because the objects themselves are not logged:
   re-run the scoreboard's `make site` from a clean checkout, which uploads
   every file and invalidates, and confirm the sign-in page's script and style
   sources against the repository.
4. **A bulk export, backup or table change on `scoreboard-devices` or
   `scoreboard-enrollments`** (`ExportTableToPointInTime`, `CreateBackup`,
   `UpdateTable`, `DeleteTable`, `UpdateContinuousBackups`, `TagResource`, …).
   These are management writes, not row access, so run the scoreboard state
   entry below too if the table itself was also touched. `aws dynamodb
   list-backups --table-name <table>` and, for an export,
   `aws dynamodb list-exports --table-arn <table arn>` give the destination
   and time; find and secure or delete anything you did not create, and treat
   every row in it as compromised the same way the scoreboard state entry
   treats a live read. `aws dynamodb describe-table --table-name <table>` and
   `aws dynamodb describe-continuous-backups --table-name <table>` confirm the
   table's billing mode and point-in-time recovery setting still match the
   scoreboard repository's `terraform/`.
5. In every case, the credential in the Actor line is the thing to cut off
   first; the direct-invoke entry's step 2 says how.

**A scoreboard state alert you cannot account for.** Assume the rows that
decide who owns a panel, or the secrets that mint a certificate, are in
somebody else's hands. In us-east-1:
1. Find what they touched. The alert gives the time and the Actor ARN; the
   record carries the table, the operation and the key. The bare filter below
   also matches the two roles' own traffic, which by the time panels exist is
   most of what these tables see, so exclude them and look at what is left:
   `aws logs filter-log-events --log-group-name /aws/cloudtrail/hockeytrack-account --start-time <ms> --end-time <ms> --filter-pattern '{ ($.eventSource = "dynamodb.amazonaws.com") && ($.eventCategory = "Data") && ($.userIdentity.sessionContext.sessionIssuer.arn != "arn:aws:iam::989232581535:role/scoreboard-api") && ($.userIdentity.sessionContext.sessionIssuer.arn != "arn:aws:iam::989232581535:role/scoreboard-enroll") }'`.
   Start one minute before the alert's time and end at least twenty minutes
   after it: the log group stamps each record when CloudTrail delivers it, not
   when the call happened.
2. Cut the credential off, as the direct-invoke entry's step 2 describes.
3. **If `scoreboard-devices` was written:** every panel's owner is now
   suspect. `aws dynamodb scan --table-name scoreboard-devices` and compare
   each row's owner with who should hold that panel. This scan is itself
   neither role, so section 14 pages on it too — expect a second alert with
   your own Actor ARN and no cause for alarm in it. A row whose owner you do
   not recognize means that panel is being driven by somebody else: put the
   right owner back, then treat the panel as theirs until its certificate is
   replaced, because the owner column does not control the device's identity.
4. **If `scoreboard-enrollments` was read:** the two secrets it holds do not
   fail the same way. The claim code is 8 characters over a 30-character
   alphabet (about 2^39.3) stored as an unsalted SHA-256 hash — brute-forceable
   offline in minutes, well inside its own rotation window — so it must be
   assumed recovered: delete the pending rows and re-enroll those panels. The
   collection token is 32 bytes of `crypto/rand`, also stored as an unsalted
   SHA-256 hash, and is not recoverable from that hash, so a read alone does
   not compromise it. A completed row's certificate is not at risk from the
   read either: enrollment is a CSR flow (the panel generates its own key
   pair and the scoreboard only signs the CSR), the private key never leaves
   the panel, and collection returns nothing but the certificate PEM.
5. **Either way, check the certificates.** `aws iot list-certificates` gives
   each one's creation date; anything created in the window that you cannot
   account for gets revoked and detached, as the admin API entry's step 8
   describes.

**A scoreboard image alert you cannot account for.** Assume the next panel
flashed or updated would run somebody else's code, and that every image on the
mirror is suspect until proven otherwise. **Do not flash or update a panel
until step 3 passes.** In us-east-1:
1. Find what they touched. The alert gives the time and the Actor ARN. Object
   writes and management writes sit in the same log group:
   `aws logs filter-log-events --log-group-name /aws/cloudtrail/hockeytrack-account --start-time <ms> --end-time <ms> --filter-pattern '{ ($.requestParameters.bucketName = "scoreboard-images-989232581535") || ($.requestParameters.id = "E1GT880VF9CHFS") || ($.requestParameters.targetDistributionId = "E1GT880VF9CHFS") || ($.requestParameters.roleName = "scoreboard-image-publisher") || ($.requestParameters.openIDConnectProviderArn = "arn:aws:iam::989232581535:oidc-provider/token.actions.githubusercontent.com") }'`.
   `bucketName` catches both the object writes and the bucket's own management
   writes: S3 data events carry it alongside `key`, which is confirmed against
   the archive's own records in this same log group. Start one minute before
   the alert's time and end at least twenty minutes after it: the log group
   stamps each record when CloudTrail delivers it, not when the call happened.
2. Cut the credential off, as the direct-invoke entry's step 2 describes. If
   the Actor is the publisher role, the session came from GitHub Actions or
   from something holding its credentials: check the `image-release`
   environment's recent deployments in the scoreboard repository for a run
   that accounts for it, and if none does, delete the role's inline policy
   before anything else — that stops further writes without waiting for the
   OIDC trust to be rewritten.
3. Prove what the mirror is serving. For every key under `images/` and for
   `latest.json`, compare the object against the GitHub release it claims to
   come from: `aws s3api list-object-versions --bucket scoreboard-images-989232581535 --prefix images/`
   shows every version and when it was written, and
   `gh release view <tag> --repo DavidJDrake/hockeytrack-scoreboard`
   gives the assets and their digests. A current version written outside a
   release run is the answer. Restore by copying the last known good version
   over the current one, as the archive entry describes; the bucket is
   versioned, so the original is almost certainly still there.
4. Check the path, not just the objects.
   `aws cloudfront get-distribution-config --id E1GT880VF9CHFS` must still show
   the images bucket as its origin, the OAC in front of it, and
   `images.scoreboard.davidjdrake.com` as its only alias. A repointed origin
   serves a different bucket under the same URL with every object untouched.
5. Check who may publish.
   `aws iam get-role --role-name scoreboard-image-publisher` and
   `aws iam list-role-policies`/`get-role-policy` against the scoreboard
   repository's `terraform/`: the trust policy must name the GitHub OIDC
   provider and the exact subject
   `repo:DavidJDrake/hockeytrack-scoreboard:environment:image-release`, with
   no second repository, no second environment and no wildcard in the subject.
   Then `aws iam get-open-id-connect-provider --open-id-connect-provider-arn arn:aws:iam::989232581535:oidc-provider/token.actions.githubusercontent.com`
   for a client ID or thumbprint you did not add — that one affects every role
   in the account that trusts GitHub, not just this one.
6. Assume any panel flashed since the write is running that image. Reflash it
   from a release you verified in step 3; its device certificate should be
   replaced too, as the state entry's step 5 describes, because whatever ran
   on it had the private key.

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
