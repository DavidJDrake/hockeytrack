# Detection for the Scoreboard's Supporting Control Plane — Design

**Status:** accepted (design approved in conversation, 2026-09-16)
**Date:** 2026-09-16
**Closes:** three gaps named in `2026-09-15-direct-invoke-design.md` §7 and in sections 10 to 12 of `terraform/security-alarms.tf`: the scoreboard functions' IAM roles, the log groups the alarms and recovery steps depend on, and the static site.
**Repository:** HockeyTrack only. One new rule (section 13), its registry entries, and the threat model. The scoreboard repository does not change.

## 1. Purpose

Sections 10 to 12 watch who may sign in, what a token is worth, and who may call the functions. Three things those rules depend on are still unwatched, and each is a way to take control or go blind without touching anything they see.

- **The roles.** `scoreboard-enroll`'s role can create IoT certificates and attach the device policy. A widened grant there mints device identities, and HockeyTrack's identity rule deliberately skips role-policy churn because its own applies are noisy.
- **The log groups.** Every alarm this project relies on is a metric filter on a log group: refused sign-ins, gate crashes, and the token mismatch. Deleting a filter silences the alarm while leaving the alarm in place, and deleting a group or shortening its retention destroys the evidence the recovery procedures read.
- **The static site.** The bucket and distribution serve the sign-in page on the real domain. A changed bucket policy or distribution origin serves a look-alike page to a signed-in owner, and no rule sees it.

## 2. Facts established by investigation (2026-09-16, read-only)

1. **The scoreboard has seven roles:** `scoreboard-api`, `scoreboard-authgate`, `scoreboard-enroll`, `scoreboard-iot-logging`, `scoreboard-reducer`, `scoreboard-scheduler-invoke`, `scoreboard-today`. All their policies are inline, so every write names the role in `requestParameters.roleName` (`PutRolePolicy`, `AttachRolePolicy`, `DeleteRolePolicy`, `UpdateAssumeRolePolicy`, `TagRole` and the rest).
2. **Six log groups matter here:** `/aws/lambda/scoreboard-{api,authgate,enroll,reducer,today}` and `/aws/apigateway/scoreboard-admin`. `AWSIotLogsV2`, which the scoreboard stack also owns, is section 8's already. `PutRetentionPolicy`, `PutMetricFilter` and `DeleteMetricFilter` all carry `requestParameters.logGroupName`.
3. **S3 writes carry `bucketName`,** and also name the bucket in `resources[].ARN`. The site bucket is `scoreboard-site-<account id>`, the same way the threat model writes it; the Terraform interpolates the account ID rather than carrying it as a literal.
4. **CloudFront names the distribution differently depending on the call.** `UpdateDistribution` and `DeleteDistribution` carry `requestParameters.id`; `CreateInvalidation` carries `requestParameters.distributionId`. The site distribution is `E3Q7R79Q7PXH26`.
5. **Ninety-day sweep of the pattern's own four sources, to 2026-09-16:** 40,420 management events scanned; none lacked a `readOnly` key. The rule as first written would have matched 1,368 of them: `CreateLogStream` 1,333, `PutRolePolicy` 9, `CreateRole` 7, `CreateLogGroup` 6, `PutRetentionPolicy` 6, `PutMetricFilter` 4, `PutBucketPolicy` 1, `PutBucketPublicAccessBlock` 1, `CreateBucket` 1. `CreateLogStream` is a Lambda cold-start side effect, not a change to anything, and it drowns out the rest by three orders of magnitude, so it is excluded by name; after that, 35 matches remain over the same 90 days, all scoreboard applies.

## 3. Design: section 13, `hockeytrack-sec-scoreboard-support`

One rule, registered in `local.security_rules` and `local.security_alert_meaning` like every other, which gives it the SNS target, dead-letter queue, topic and queue policies, and its own alert sentence.

```hcl
event_pattern = jsonencode({
  "detail-type" = ["AWS API Call via CloudTrail"]
  "detail" = {
    "eventSource"   = ["iam.amazonaws.com", "logs.amazonaws.com", "s3.amazonaws.com", "cloudfront.amazonaws.com"]
    "eventCategory" = ["Management"]
    "readOnly"      = [false]
    "eventName"     = [{ "anything-but" = ["CreateLogStream"] }]
    "$or" = [
      { "requestParameters" = { "roleName" = local.scoreboard_role_names } },
      { "requestParameters" = { "logGroupName" = [{ "wildcard" = "/aws/lambda/scoreboard-*" }, "/aws/apigateway/scoreboard-admin"] } },
      { "requestParameters" = { "bucketName" = [local.scoreboard_site_bucket] } },
      { "requestParameters" = { "id" = [local.scoreboard_site_distribution] } },
      { "requestParameters" = { "Resource" = [{ "prefix" = "arn:aws:cloudfront::${data.aws_caller_identity.current.account_id}:distribution/${local.scoreboard_site_distribution}" }] } },
      { "resources" = { "ARN" = ["arn:aws:s3:::${local.scoreboard_site_bucket}"] } },
    ]
  }
})
```

- **The roles are looked up, not written as literals.** A `data "aws_iam_role"` per name fails the plan if one disappears, the way sections 10 to 12 look up their pool, API and functions. The bucket and distribution come from `data "aws_s3_bucket"` and a `data "aws_cloudfront_distribution"` lookup by ID, with a precondition that the ID's alias is the site's domain, so a replaced distribution is caught at HockeyTrack's next apply rather than silently watched.
- **Site deploys stay silent without naming an event.** `CreateInvalidation` carries `distributionId`, which this pattern does not match, while configuration changes carry `id`, which it does. So `make site` produces no alert, and the rule keeps sections 9 to 12's "no event names" shape, with one named exception: `CreateLogStream` is excluded by name at the top level, because a Lambda cold start creates a log stream and names its group on every invocation, and the field it carries the group name in is the same one every real log-group change carries it in, so nothing distinguishes the two. If CloudFront ever records an invalidation under `id`, deploys start paging; the comment says so.
- **Accepted noise:** the sweep's 35 post-exclusion matches in 90 days, all scoreboard applies, roughly one every two or three days: `PutRolePolicy` 9, `CreateRole` 7, `CreateLogGroup` 6, `PutRetentionPolicy` 6, `PutMetricFilter` 4, `PutBucketPolicy` 1, `PutBucketPublicAccessBlock` 1, `CreateBucket` 1. No `UpdateDistribution` on the site's own distribution landed in the window — the earlier 17 was an account-wide count across every distribution, not this one — so the site-change noise is unmeasured rather than zero by nature; the pattern still admits one when it happens. All are deliberate, and the person making the change is the person reading the alert.
- **Length precondition** `<= 2048`, like sections 9 to 12.

Alert sentence: *If this was not you, assume the scoreboard's supporting resources have been changed: a function's role, the log groups its alarms and recovery steps read, the site's bucket or distribution, or a bulk export, backup or restore point on scoreboard-devices or scoreboard-enrollments. Check the enroll role's IoT permissions, the metric filters and retention on every scoreboard log group, the site bucket's policy and the distribution's origins and behaviors, and where any export or backup landed, against the scoreboard repository.* (Extended in the devices-data-plane whole-branch review; see the section below.)

## 4. What it does not see

- **Objects in the site bucket.** `PutObject` is a data event, and the trail logs object events only for the raw archive. A page replaced in place is invisible; the distribution's origin and behaviors are what this rule watches.
- **A moved domain alias.** `AssociateAlias` names the target distribution, which for an attacker's copy is not this one, and the DNS record itself lives outside this account's watched resources.
- **Customer-managed policies.** These roles use inline policies only, so `CreatePolicyVersion` on a managed policy attached later would name only a policy ARN.
- **Identities that can already reach these resources,** which section 1 covers for the account's own escalation paths.
- **Rewriting this rule,** which section 9 catches.
- **A log stream created to impersonate a log source.** `CreateLogStream` is excluded account-wide within this rule's four sources, so one crafted to look like a cold start does not page either.

## 5. Testing

- **Before applying:** `test-event-pattern` on real events — a `PutRolePolicy` on `scoreboard-authgate`, a `PutRetentionPolicy` and a `PutMetricFilter` on a scoreboard log group, and an `UpdateDistribution` on the site distribution must match; a `PutRolePolicy` on an EbookShare role, a `PutBucketPolicy` on another project's bucket, a `CreateInvalidation` on the site distribution, a `CreateLogStream` on a scoreboard log group, and every read must not. Then a 90-day sweep with a local matcher, spot-checked, including events with no `readOnly` key.
- **Live breaks,** each harmless and each expected to email: tag and untag `scoreboard-enroll`'s role; re-save `/aws/lambda/scoreboard-api`'s retention at 30 days; re-save the site bucket's current policy.
- **Negatives:** a site deploy (`make site`, which invalidates) and a scoreboard plan produce no email.
- **After:** metrics, DLQ depth, drift checks, and a verification record in this spec.

## 6. The threat model

- **§4** gains a paragraph covering all three resources and naming what stays unwatched.
- **§7** gains a recovery entry: from the record, identify which of the three was touched; for a role, compare its inline policies with the scoreboard repository and check IoT certificates created since; for a log group, check the filters and retention against the repository and whether an alarm has been in INSUFFICIENT_DATA since; for the site, compare the bucket policy and the distribution's origins, behaviors and aliases, then invalidate and redeploy from the repository.

## 7. Verification record (2026-09-16)

All times UTC. The `test-event-pattern` set below is the pre-exclusion test
set: it does not include a `CreateLogStream` case, because the exclusion was
the sweep's own finding, made after this record's "Before applying" section
was first written. It is added below, verified live in the fix round that
closed I1/I2 (`.superpowers/sdd/2026-09-16-control-plane-detection/final-fix-report.md`
carries that round's full table against the rule as fixed).

### Before applying

- **The pattern is 874 characters,** well inside the 2048 the precondition enforces.
- **`test-event-pattern` against real events, 8 of 8 as expected.** Matching: `PutRolePolicy` on `scoreboard-authgate`, `PutRetentionPolicy` on `/aws/lambda/scoreboard-authgate`, `PutMetricFilter` on `/aws/lambda/scoreboard-enroll`, and a real `UpdateDistribution` with its `id` swapped to the site's. Not matching: `PutRolePolicy` on an EbookShare role, `PutBucketPolicy` on another project's bucket, a real `CreateInvalidation` on the site's distribution, and a read (`GetBucketPolicy`) on the site bucket.
- **`CreateLogStream` negative, added in the fix round, 9 of 9 total.** A synthetic `CreateLogStream` naming a scoreboard log group in `logGroupName` was checked with `test-event-pattern` against the fixed rule's pattern (I1/I2 applied, not yet re-applied to AWS) and did not match, confirming the top-level exclusion does what the comment says; the fix round did not touch that exclusion, so the result holds for the deployed rule too. It was not part of the original 8; the scoreboard-plan negative in §5 ("Negatives: a site deploy … and a scoreboard plan produce no email") was likewise never run as its own `test-event-pattern` case — only the deploy negative was, at 01:46:34 below. The scoreboard-plan negative is instead covered by the Drift entry: a scoreboard `terraform plan` after this rule's apply reads these resources with `Describe`/`Get`/`List` calls, which are `readOnly true` and so never reach an `ENABLED` rule at all (the same reasoning section 9 gives), and the repository's own drift check exiting 0 confirms nothing about that plan paged.
- **The 90-day sweep changed the rule.** Scanning all 40,420 management events from the four sources over the 90 days to 2026-09-16 — none of them lacking a `readOnly` key — the rule as first written would have matched 1,368. Of those, 1,333 were `CreateLogStream`: every Lambda cold start creates a stream, and the call names the group. Fifteen pages a day would have trained the reader to ignore the rule, so `CreateLogStream` is the one event name the pattern excludes, and the comment says what that costs. The remaining 35 are `PutRolePolicy` 9, `CreateRole` 7, `CreateLogGroup` 6, `PutRetentionPolicy` 6, `PutMetricFilter` 4, `PutBucketPolicy` 1, `PutBucketPublicAccessBlock` 1 and `CreateBucket` 1 — all scoreboard applies, about one every two or three days.

### Applied 2026-09-16 01:35

`Plan: 2 to add, 2 to change, 0 to destroy` — the rule and its SNS target added, the topic and dead-letter queue policies updated.

### Breaks, 01:37:40 to 01:37:55

Each re-saves a value that was already set, so nothing changed: a tag added and removed on `scoreboard-enroll`'s role, `/aws/lambda/scoreboard-api`'s retention re-saved at 30 days, and the site bucket's current policy re-saved. Afterwards the role had no tags, retention was still 30, and the policy still had its single statement.

All four paged. `MatchedEvents` 4, `Invocations` 4, no `FailedInvocations`, and the dead-letter queue at 0. The emails arrived between 01:37:52 and 01:38:13, each carrying the section 13 sentence, for example *HOCKEYTRACK SECURITY: PutRetentionPolicy … Actor: …user/funandgames*.

### Negative, 01:46:34

A CloudFront invalidation on the site's distribution — what every `make site` deploy issues — was recorded by CloudTrail and matched nothing. The rule's `MatchedEvents` stayed empty for that window. Deploys are silent because an invalidation names the distribution in `distributionId` while a configuration change names it in `id`.

### Drift

`terraform plan -detailed-exitcode` exits 0 in this repository after the apply.

### After the whole-branch review (2026-09-16 03:00)

The review found two gaps in the rule as first applied, and four documents that had drifted apart. Both gaps are closed, and the pattern is now 1,572 characters.

- **The CloudFront tag branch was dead.** It matched `Resource`, but CloudTrail records CloudFront's request parameters with a lowercase first letter, so tagging the distribution paged nobody. Worse, the field table listed that row as confirmed against real events when it came from the service model: no CloudFront `TagResource` occurred in the 90-day window. Both casings now match, and the row says where it came from.
- **The log-group leg saw one field name only.** CloudWatch Logs names a group through `logGroupName`, `logGroupIdentifier` (name or ARN) and `resourceArn` depending on the call. `PutTransformer` and `PutDataProtectionPolicy` rewrite or mask log content at ingestion, which silences the refusal, crash and mismatch filters exactly as deleting a filter would, and neither paged. Both fields are now matched in both forms. The list-valued fields on query and anomaly-detector calls stay out of the pattern and are named as gaps.
- **EventBridge rejects a pattern with too many wildcards in one field,** and only at apply, where `terraform validate` cannot see it. The ARN branches use `prefix` with the account and region written out instead.
- **Breaks for the newly covered routes, 03:04:13 to 03:04:18:** a tag added and removed on the distribution, and a data-protection policy put and deleted on `/aws/lambda/scoreboard-api`. All four paged: `MatchedEvents` 4, `Invocations` 4, no `FailedInvocations`, dead-letter queue 0. Nothing was left behind — the distribution has no tags, and the policy is deleted.
- **The documents were reconciled.** The threat model no longer says the rule lists no event names, and the six places in sections 10, 11 and the threat model that called these routes unwatched now point at section 13.

### The devices-data-plane whole-branch review (2026-09-16)

That review found a bulk-read bypass: `ExportTableToPointInTime` dumps a whole
table to S3 as a management event, producing no data event at all, so section
14 (which only sees data events) cannot see it; `CreateBackup` plus
`RestoreTableFromBackup`/`RestoreTableToPointInTime` are the same shape.
Point-in-time recovery is enabled on `scoreboard-enrollments` today, so this
was live, not hypothetical.

- **`eventSource` gained `dynamodb.amazonaws.com`,** and two new `$or`
  branches were added: `requestParameters.tableName` against the two table
  names (`CreateBackup`, `UpdateTable`, `DeleteTable`,
  `UpdateContinuousBackups`) and `requestParameters.tableArn` against the two
  table ARNs (`ExportTableToPointInTime`, the one write that carries no
  `TableName`). The existing `resourceArn` branch also gained the two table
  ARNs, since DynamoDB's `TagResource` carries only `ResourceArn`, the same
  field name the log-group branch already matched. Both new locals reuse
  `data.aws_dynamodb_table.scoreboard_state` (`cloudtrail.tf`) rather than
  adding a second lookup of the same tables.
- **The pattern grew from 1,572 to 1,994 characters,** still well inside 2048.
- **`test-event-pattern`, 17 of 17 as expected:** `ExportTableToPointInTime`
  and `CreateBackup` on `scoreboard-enrollments` matched; `UpdateTable` on
  `scoreboard-devices` matched; the same three calls on a third table
  (`scoreboard-games`) did not; a DynamoDB data event (`GetItem`) did not,
  since `eventCategory` stays `Management`; and the rule's eight pre-existing
  positives and negatives (`PutRolePolicy`, `PutRetentionPolicy`,
  `PutBucketPolicy`, `UpdateDistribution`, `PutTransformer`, `CreateLogStream`,
  `CreateInvalidation`) were unaffected.
- **What still isn't seen:** a restore. `RestoreTableFromBackup` names the
  source only as `BackupArn` and the copy as `TargetTableName`;
  `RestoreTableToPointInTime` does name the source table, but as
  `SourceTableName`/`SourceTableArn`, not `TableName`/`TableArn`, so neither
  field this rule matches sees it. The restored copy is a new table either
  way, outside both this rule and section 14 until something is pointed at it
  by name.
- **The alert sentence and the threat model's recovery entry (§7, "A
  scoreboard support alert you cannot account for") were extended** to name
  the bulk-route case alongside the role, log-group and site cases.
- Not yet applied or broken live; the pattern is checked offline
  (`terraform validate`, `test-event-pattern`) pending the next apply.

### Extended to DynamoDB management writes (2026-09-16 16:48)

The devices-data-plane review found a bulk-read path nothing watched: point-in-time recovery is enabled on `scoreboard-enrollments`, and `ExportTableToPointInTime` copies the whole table to S3 as a management event, producing no row-level events at all. `CreateBackup` and the restore calls are the same shape, and no rule in this repository watched `dynamodb.amazonaws.com` management events.

Section 13 now covers them: `dynamodb.amazonaws.com` joins its event sources, with branches on the two tables' names and ARNs. The pattern grew from 1,572 to 1,994 characters, against the 2,048 the precondition enforces — worth knowing before anything else is added to it.

**Verified live.** Before applying, 20 `test-event-pattern` cases: `ExportTableToPointInTime`, `CreateBackup` and `UpdateTable` match on the two tables and not on a third; a DynamoDB row event does not match, because section 14 owns those; and every earlier case still behaves as it did. After applying, a tag added and removed on `scoreboard-enrollments` produced four matches on section 13 — CloudTrail recorded the untag three times — and none on section 14. The table has no tags left, the dead-letter queue is empty, and a sign-in six minutes later paged nothing.

What still is not watched: a restore names a new table, so the restored copy falls outside both rules.
