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
3. **S3 writes carry `bucketName`,** and also name the bucket in `resources[].ARN`. The site bucket is `scoreboard-site-989232581535`.
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
- **Accepted noise:** scoreboard applies that change a role's policy (about 10 in 90 days), a retention or metric-filter change (6 in 90 days), and `UpdateDistribution` (17 in 90 days, all while the site was being built). All are deliberate, and the person doing them is the person reading the alert.
- **Length precondition** `<= 2048`, like sections 9 to 12.

Alert sentence: *If this was not you, assume the scoreboard's supporting resources have been changed: a function's role, the log groups its alarms and recovery steps read, or the site's bucket or distribution. Check the enroll role's IoT permissions, the metric filters and retention on every scoreboard log group, and the site bucket's policy and the distribution's origins and behaviors, against the scoreboard repository.*

## 4. What it does not see

- **Objects in the site bucket.** `PutObject` is a data event, and the trail logs object events only for the raw archive. A page replaced in place is invisible; the distribution's origin and behaviors are what this rule watches.
- **A moved domain alias.** `AssociateAlias` names the target distribution, which for an attacker's copy is not this one, and the DNS record itself lives outside this account's watched resources.
- **Customer-managed policies.** These roles use inline policies only, so `CreatePolicyVersion` on a managed policy attached later would name only a policy ARN.
- **Identities that can already reach these resources,** which section 1 covers for the account's own escalation paths.
- **Rewriting this rule,** which section 9 catches.

## 5. Testing

- **Before applying:** `test-event-pattern` on real events — a `PutRolePolicy` on `scoreboard-authgate`, a `PutRetentionPolicy` and a `PutMetricFilter` on a scoreboard log group, and an `UpdateDistribution` on the site distribution must match; a `PutRolePolicy` on an EbookShare role, a `PutBucketPolicy` on another project's bucket, a `CreateInvalidation` on the site distribution, a `CreateLogStream` on a scoreboard log group, and every read must not. Then a 90-day sweep with a local matcher, spot-checked, including events with no `readOnly` key.
- **Live breaks,** each harmless and each expected to email: tag and untag `scoreboard-enroll`'s role; re-save `/aws/lambda/scoreboard-api`'s retention at 30 days; re-save the site bucket's current policy.
- **Negatives:** a site deploy (`make site`, which invalidates) and a scoreboard plan produce no email.
- **After:** metrics, DLQ depth, drift checks, and a verification record in this spec.

## 6. The threat model

- **§4** gains a paragraph covering all three resources and naming what stays unwatched.
- **§7** gains a recovery entry: from the record, identify which of the three was touched; for a role, compare its inline policies with the scoreboard repository and check IoT certificates created since; for a log group, check the filters and retention against the repository and whether an alarm has been in INSUFFICIENT_DATA since; for the site, compare the bucket policy and the distribution's origins, behaviors and aliases, then invalidate and redeploy from the repository.
