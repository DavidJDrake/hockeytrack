# Scoreboard Supporting Control Plane Detection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Page on any write to the scoreboard's seven IAM roles, its six log groups, or the static site's bucket and distribution.

**Architecture:** One EventBridge rule (section 13) on CloudTrail management events, scoped by resource with no event names, registered in the existing rule registry so it inherits the SNS target, DLQ and policies. Resources are looked up by data source so a rename fails the plan. Site deploys stay silent because `CreateInvalidation` names the distribution in a field the pattern does not match.

**Tech Stack:** Terraform (AWS provider), EventBridge, CloudTrail, SNS, aws-cli.

**Spec:** `docs/superpowers/specs/2026-09-16-control-plane-detection-design.md` (this repository)

## Global Constraints

- **Repository is PUBLIC.** Never read, modify or commit `terraform/terraform.tfvars`. No credentials, no real email addresses.
- **Agents never run** `terraform apply`, `terraform plan`, any mutating AWS call, `make deploy`, or `git push`. `terraform -chdir=terraform fmt -check`, `terraform -chdir=terraform validate` and read-only `aws ... --region us-east-1` calls are allowed.
- **Every AWS CLI command passes `--region us-east-1`.**
- **US spelling** in prose, identifiers and resource names.
- **Rule naming and shape:** `hockeytrack-sec-scoreboard-support`, registered in `local.security_rules` and `local.security_alert_meaning`, scoped by resource, no `eventName` constraints, `readOnly = [false]`, `eventCategory = ["Management"]`, pattern `<= 2048` characters enforced by a `precondition`.
- **Alert sentences are plain text with no double quotes** (they are embedded in a JSON template).
- **The seven roles:** `scoreboard-api`, `scoreboard-authgate`, `scoreboard-enroll`, `scoreboard-iot-logging`, `scoreboard-reducer`, `scoreboard-scheduler-invoke`, `scoreboard-today`.
- **The log groups:** `/aws/lambda/scoreboard-*` (wildcard) and `/aws/apigateway/scoreboard-admin`. `AWSIotLogsV2` belongs to section 8 and stays out.
- **The site:** bucket `scoreboard-site-<account id>`, distribution `E3Q7R79Q7PXH26`, alias `scoreboard.davidjdrake.com`.
- **Branch:** `control-plane-detection` in `/home/jay/projects/hockeytrack` (already checked out; the spec is committed there).

## File map

| File | Responsibility |
|---|---|
| `terraform/security-alarms.tf` (modify) | Section 13: data sources, locals, the rule; registry entries in `local.security_rules` and `local.security_alert_meaning` |
| `docs/threat-model.md` (modify) | §4 paragraph; §7 recovery entry |

---

### Task 1: Section 13 and the threat model

**Files:**
- Modify: `terraform/security-alarms.tf` (registry near lines 130 and 173; new section after `aws_cloudwatch_event_rule.scoreboard_invoke`, which ends around line 1420)
- Modify: `docs/threat-model.md` (§4, after the paragraph beginning `**Calling the scoreboard's admin functions directly pages someone`; §7, after the last numbered step of the entry beginning `**A scoreboard direct-invoke alert you cannot account for.**` and before `**The archive has lost objects.**`)

**Interfaces:**
- **Consumes:** `data.aws_caller_identity.current` (declared in `terraform/data.tf`); the registry locals `local.security_rules` and `local.security_alert_meaning`; the target and policies that loop over them.
- **Produces, for Task 2:** `aws_cloudwatch_event_rule.scoreboard_support` named `hockeytrack-sec-scoreboard-support`, and `local.scoreboard_support_pattern`.

- [ ] **Step 1: Add the data sources, locals and rule**

Append to `terraform/security-alarms.tf`, after the `scoreboard_invoke` rule:

```hcl
# ---- 13. The scoreboard's supporting control plane ----
#
# Sections 10 to 12 watch who may sign in, what a token is worth, and who may
# call the functions. Each of them leans on three things nothing watched until
# now, and each is a way to take control or go blind without touching what
# those rules see:
#
#   The roles          scoreboard-enroll's role can create IoT certificates and
#                      attach the device policy, so a widened grant there mints
#                      device identities. Section 1 excludes role-policy churn
#                      deliberately, because this account's own applies are
#                      noisy; scoped to these seven roles it is not.
#   The log groups     Every alarm here is a metric filter on a log group:
#                      refused sign-ins, gate crashes, the token mismatch.
#                      Deleting a filter silences the alarm while leaving the
#                      alarm in place, and deleting a group or shortening its
#                      retention destroys what the recovery procedures read.
#   The static site    The bucket and distribution serve the sign-in page on
#                      the real domain. A changed bucket policy or a repointed
#                      origin serves a look-alike page to the owner.
#
# Which fields name them, confirmed against real events on 2026-09-16:
#
#   roleName       every IAM write that takes a role; these roles' policies are
#                  all inline, so no write names them only by policy ARN
#   logGroupName   PutRetentionPolicy, PutMetricFilter, DeleteMetricFilter,
#                  DeleteLogGroup, PutSubscriptionFilter
#   bucketName     S3 bucket writes, which also name the bucket in resources[]
#   id             CloudFront UpdateDistribution and DeleteDistribution
#   Resource       CloudFront TagResource/UntagResource, a distribution ARN
#
# Site deploys stay silent without naming an event, which is how sections 9 to
# 12 are built: CreateInvalidation names the distribution in distributionId,
# which this pattern does not match, while configuration changes name it in id,
# which it does. If CloudFront ever records an invalidation under id, every
# deploy starts paging -- noisy, not blind, and the fix is a field list here.
#
# Measured over ninety days to 2026-09-16, account-wide: CreateInvalidation
# past the 50-event query cap, UpdateDistribution 17 (all while the site was
# being built), PutRolePolicy 50 of which about 10 name scoreboard roles,
# PutRetentionPolicy 27, PutMetricFilter 6, PutBucketPolicy 13 (none on the
# site bucket), DeleteLogGroup 0. So the accepted noise is a scoreboard apply
# that changes a role policy, a retention or filter change, and the occasional
# distribution change. All are deliberate, and the person doing them is the
# person reading the alert.
#
# What it does not see:
#   - Objects in the site bucket. PutObject is a data event, and the trail logs
#     object events only for the raw archive, so a page replaced in place is
#     invisible. The distribution's origins and behaviors are what this covers.
#   - A moved domain alias. AssociateAlias names the target distribution, which
#     for an attacker's copy is not this one, and the DNS record lives outside
#     the resources this account watches.
#   - Customer-managed policies. These roles use inline policies only; a
#     managed policy attached later could be widened by a CreatePolicyVersion
#     that names only the policy ARN.
#   - Identities that can already reach these resources, which section 1 covers
#     for the account's own escalation paths.
#   - Rewriting this rule, which section 9 catches.
data "aws_iam_role" "scoreboard" {
  for_each = toset([
    "scoreboard-api",
    "scoreboard-authgate",
    "scoreboard-enroll",
    "scoreboard-iot-logging",
    "scoreboard-reducer",
    "scoreboard-scheduler-invoke",
    "scoreboard-today",
  ])
  name = each.key
}

data "aws_s3_bucket" "scoreboard_site" {
  bucket = "scoreboard-site-${data.aws_caller_identity.current.account_id}"
}

data "aws_cloudfront_distribution" "scoreboard_site" {
  id = "E3Q7R79Q7PXH26"
}

locals {
  scoreboard_role_names        = sort([for r in data.aws_iam_role.scoreboard : r.name])
  scoreboard_site_bucket       = data.aws_s3_bucket.scoreboard_site.bucket
  scoreboard_site_distribution = data.aws_cloudfront_distribution.scoreboard_site.id
  scoreboard_site_alias        = "scoreboard.davidjdrake.com"

  scoreboard_support_pattern = jsonencode({
    "detail-type" = ["AWS API Call via CloudTrail"]
    "detail" = {
      "eventSource"   = ["iam.amazonaws.com", "logs.amazonaws.com", "s3.amazonaws.com", "cloudfront.amazonaws.com"]
      "eventCategory" = ["Management"]
      "readOnly"      = [false]
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
}

resource "aws_cloudwatch_event_rule" "scoreboard_support" {
  name          = "hockeytrack-sec-scoreboard-support"
  description   = "Any write naming a scoreboard function's role, a scoreboard log group, or the static site's bucket or distribution: the routes to widening a role, silencing an alarm, or serving a look-alike page"
  event_pattern = local.scoreboard_support_pattern

  lifecycle {
    precondition {
      condition     = contains(data.aws_cloudfront_distribution.scoreboard_site.aliases, local.scoreboard_site_alias)
      error_message = "Distribution ${local.scoreboard_site_distribution} does not serve ${local.scoreboard_site_alias}. The scoreboard site's distribution has been replaced, and this rule would watch the wrong one."
    }
    precondition {
      condition     = length(local.scoreboard_support_pattern) <= 2048
      error_message = "The scoreboard support rule's event pattern is ${length(local.scoreboard_support_pattern)} characters. EventBridge rejects patterns over 2048, and only at apply."
    }
  }
}
```

- [ ] **Step 2: Register the rule**

In `local.security_rules`, after the `scoreboard_invoke` line:

```hcl
    scoreboard_support = aws_cloudwatch_event_rule.scoreboard_support
```

In `local.security_alert_meaning`, after its `scoreboard_invoke` line:

```hcl
    scoreboard_support = "If this was not you, assume the scoreboard's supporting resources have been changed: a function's role, the log groups its alarms and recovery steps read, or the site's bucket or distribution. Check the enroll role's IoT permissions, the metric filters and retention on every scoreboard log group, and the site bucket's policy and the distribution's origins and behaviors, against the scoreboard repository."
```

Keep `terraform fmt` alignment in both maps.

- [ ] **Step 3: Check the pattern's length offline**

The preconditions only run at plan time, which you cannot run. Compute the rendered length yourself with a small Python script that builds the same JSON, using the real values: the seven role names sorted, bucket `scoreboard-site-989232581535`, distribution `E3Q7R79Q7PXH26`, account `989232581535`. Use compact separators, the way `jsonencode` renders. Report the number; it must be `<= 2048`.

- [ ] **Step 4: Validate**

Run:
```bash
cd /home/jay/projects/hockeytrack
terraform -chdir=terraform fmt -check && terraform -chdir=terraform validate
```
Expected: `fmt` silent (run `terraform -chdir=terraform fmt` on the file and re-check if it is not), `validate` prints "Success!". Do not run `plan`.

- [ ] **Step 5: Add the §4 paragraph**

In `docs/threat-model.md`, after the paragraph beginning `**Calling the scoreboard's admin functions directly pages someone, and forged claims are refused.**`, add:

```markdown
**Changing what the scoreboard's rules lean on pages someone.** Three things
carry the rules above, and none of them was watched. The seven roles the
scoreboard's functions assume, `scoreboard-enroll`'s above all, which may
create IoT certificates and attach the device policy. The log groups whose
metric filters are the refusal, crash and mismatch alarms, and whose history
the recovery procedures read: deleting a filter silences an alarm without
touching it, and shortening retention destroys the evidence. And the static
site's bucket and distribution, which serve the sign-in page on the real
domain. A fourth rule now fires on any write naming one of them. It is scoped
to those resources and lists no event names, so ordinary site deploys stay
silent only because an invalidation names the distribution in a different
field than a configuration change does. It does not see objects replaced
inside the site bucket, which are data events the trail does not log, nor the
DNS record that points the domain at the distribution.
```

- [ ] **Step 6: Add the §7 recovery entry**

In `docs/threat-model.md` §7, after the last numbered step of the entry beginning `**A scoreboard direct-invoke alert you cannot account for.**` and before `**The archive has lost objects.**`, add:

```markdown
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
4. In every case, the credential in the Actor line is the thing to cut off
   first; the direct-invoke entry's step 2 says how.
```

- [ ] **Step 7: Commit**

```bash
git add terraform/security-alarms.tf docs/threat-model.md
git commit -m "security: watch the scoreboard's roles, log groups and static site

Section 13, hockeytrack-sec-scoreboard-support: any write naming one of the
seven scoreboard roles, a scoreboard log group, or the site's bucket or
distribution. Site deploys stay silent because an invalidation names the
distribution in distributionId, which the pattern does not match.

Co-Authored-By: <your model> <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Ckux9bzkCV2K2XAp8HkM2i"
```

---

### Task 2: Proof, apply and record (controller and user; no subagent)

- [ ] **Step 1: Plan**

```bash
cd /home/jay/projects/hockeytrack
tag=$(aws lambda get-function --region us-east-1 --function-name hockeytrack-poller --query Code.ImageUri --output text | sed 's/.*://')
terraform -chdir=terraform plan -input=false -no-color -var="image_tag=$tag" -out=<scratchpad>/support.tfplan
terraform -chdir=terraform show -no-color <scratchpad>/support.tfplan | grep -E "will be|must be|Plan:"
```

Expected: `Plan: 2 to add, 2 to change, 0 to destroy.` — the rule and `aws_cloudwatch_event_target.security["scoreboard_support"]` added, the SNS topic policy and the DLQ queue policy changed. Anything else stops the task.

- [ ] **Step 2: Test the pattern against real events before applying**

Rebuild the rendered pattern from the plan output into `<scratchpad>/support-pattern.json`. Then pull real events with `aws cloudtrail lookup-events` and wrap each as:

```json
{"version":"0","id":"t","detail-type":"AWS API Call via CloudTrail","source":"aws.<service>","account":"989232581535","time":"<eventTime>","region":"us-east-1","resources":[],"detail":<record>}
```

**Must match:** `PutRolePolicy` on `scoreboard-authgate`; `PutRetentionPolicy` on `/aws/lambda/scoreboard-authgate`; `PutMetricFilter` on `/aws/lambda/scoreboard-enroll`; `UpdateDistribution` on `E3Q7R79Q7PXH26` (if none in 90 days, build one from a real `UpdateDistribution` on another distribution with `id` swapped).

**Must not match:** `PutRolePolicy` on an EbookShare role; `PutBucketPolicy` on another project's bucket; `CreateInvalidation` on `E3Q7R79Q7PXH26`; any read-only event; an event with no `readOnly` key.

**Then sweep:** 90 days of `lookup-events`, filtered locally with the same matcher, spot-checking a sample against `test-event-pattern`, and count what would have paged.

- [ ] **Step 3: The user applies**

`terraform -chdir=/home/jay/projects/hockeytrack/terraform apply <scratchpad>/support.tfplan`

Expected: the section 9 rewrite rule emails for `PutRule` and `PutTargets`.

- [ ] **Step 4: Breaks**

Each is harmless and reversible, run by the user or on their explicit say-so:

```bash
aws iam tag-role --region us-east-1 --role-name scoreboard-enroll --tags Key=detection-check,Value=2026-09-16
aws iam untag-role --region us-east-1 --role-name scoreboard-enroll --tag-keys detection-check
aws logs put-retention-policy --region us-east-1 --log-group-name /aws/lambda/scoreboard-api --retention-in-days 30
aws s3api get-bucket-policy --region us-east-1 --bucket scoreboard-site-989232581535 --query Policy --output text > <scratchpad>/site-policy.json
aws s3api put-bucket-policy --region us-east-1 --bucket scoreboard-site-989232581535 --policy file://<scratchpad>/site-policy.json
```

The retention value and the bucket policy are re-saved as they already are, so nothing changes. Expect four emails (tag, untag, retention, bucket policy), each with the section 13 sentence.

- [ ] **Step 5: Negatives**

- A scoreboard `make site` deploy (the user runs it) must produce no email, and `hockeytrack-sec-scoreboard-support`'s `MatchedEvents` must not rise for the invalidation.
- A scoreboard `terraform plan` produces no email.

- [ ] **Step 6: Health and drift**

```bash
for m in MatchedEvents Invocations FailedInvocations; do
  aws cloudwatch get-metric-statistics --region us-east-1 --namespace AWS/Events --metric-name $m --dimensions Name=RuleName,Value=hockeytrack-sec-scoreboard-support --start-time <ISO> --end-time <ISO> --period 300 --statistics Sum --query 'Datapoints[].Sum'
done
aws sqs get-queue-attributes --region us-east-1 --queue-url "$(aws sqs get-queue-url --region us-east-1 --queue-name hockeytrack-security-alerts-dlq --query QueueUrl --output text)" --attribute-names ApproximateNumberOfMessages
terraform -chdir=terraform plan -input=false -var="image_tag=$tag" -detailed-exitcode
```

Expected: matches equal the breaks, no failed invocations, DLQ 0, plan exit 0.

- [ ] **Step 7: Verification record**

Append `## 7. Verification record (<date>)` to the spec with the pattern test table, the sweep count, the breaks and their emails, the negatives, the metrics and the drift check. Commit it.

- [ ] **Step 8: Finish**

Use superpowers:finishing-a-development-branch.
