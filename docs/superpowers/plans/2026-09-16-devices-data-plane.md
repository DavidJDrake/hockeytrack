# Devices and Enrollments Data-Plane Detection Implementation Plan

> **Superseded during implementation** in two places (the `exists:false` leaf and the dropped `tableName` gate) — `docs/superpowers/specs/2026-09-16-devices-data-plane-design.md` is the authority.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Page on any read or write of the `scoreboard-devices` or `scoreboard-enrollments` rows that was not made by the `scoreboard-api` or `scoreboard-enroll` role.

**Architecture:** A third CloudTrail event selector logs DynamoDB data events for the two tables; section 14 pages on any of those events whose session issuer is not one of the two function roles, including callers with no session at all.

**Tech Stack:** Terraform (AWS provider), CloudTrail data events, EventBridge, SNS, aws-cli.

**Spec:** `docs/superpowers/specs/2026-09-16-devices-data-plane-design.md`

## Global Constraints

- **Repository is PUBLIC.** Never read, modify or commit `terraform/terraform.tfvars`. No credentials, no real email addresses.
- **Agents never run** `terraform apply`, `terraform plan`, any mutating AWS call, or `git push`. `terraform -chdir=terraform fmt -check`, `terraform -chdir=terraform validate`, read-only `aws ... --region us-east-1` calls and `aws events test-event-pattern` are allowed.
- **Every AWS CLI command passes `--region us-east-1`.** US spelling.
- **Rule shape:** `hockeytrack-sec-scoreboard-state`, registered in `local.security_rules` and `local.security_alert_meaning`, no `eventName` constraints, `eventCategory = ["Data"]`, pattern `<= 2048` characters enforced by a precondition.
- **Alert sentences are plain text with no double quotes.**
- **The tables:** `scoreboard-devices`, `scoreboard-enrollments`. **The allowed roles:** `scoreboard-api`, `scoreboard-enroll`.
- **Branch:** `devices-data-plane-detection` in `/home/jay/projects/hockeytrack` (checked out; spec committed).
- **Do not weaken any existing rule.** Sections 9 to 13 keep working exactly as they do.

## File map

| File | Responsibility |
|---|---|
| `terraform/cloudtrail.tf` (modify) | `data "aws_dynamodb_table"` lookups; third `event_selector` |
| `terraform/security-alarms.tf` (modify) | Section 14: locals, rule, registry entries |
| `docs/threat-model.md` (modify) | §4 paragraph; §7 recovery entry |

---

### Task 1: The selector, section 14, and the threat model

**Files:**
- Modify: `terraform/cloudtrail.tf` (after the Lambda `event_selector` in `aws_cloudtrail.account`; new data source beside `data "aws_lambda_function" "scoreboard_admin_path"`)
- Modify: `terraform/security-alarms.tf` (registry maps near the top; new section after the section 13 rule, at the end of the file)
- Modify: `docs/threat-model.md` (§4 after the paragraph beginning `**Changing what the scoreboard's rules lean on pages someone.**`; §7 after the last step of the entry beginning `**A scoreboard support alert you cannot account for.**`)

**Interfaces:**
- **Consumes:** `data.aws_caller_identity.current`; the registry locals and the target/policies that loop over them.
- **Produces, for Task 2:** `aws_cloudwatch_event_rule.scoreboard_state` named `hockeytrack-sec-scoreboard-state`, and `local.scoreboard_state_pattern`.

- [ ] **Step 1: Look up the tables and log their data events**

In `terraform/cloudtrail.tf`, beside the existing `data "aws_lambda_function" "scoreboard_admin_path"`:

```hcl
# The two tables that decide who owns a panel and which enrollment secrets are
# live. Looked up by name so a renamed table fails this plan instead of
# silently logging nothing, the way the admin-path functions are.
data "aws_dynamodb_table" "scoreboard_state" {
  for_each = toset(["scoreboard-devices", "scoreboard-enrollments"])
  name     = each.key
}
```

Inside `aws_cloudtrail.account`, after the Lambda `event_selector`:

```hcl
  # Row-level access to the scoreboard's state. Writes hand a panel to a
  # different owner without any API call; reads hand over the collection-token
  # and claim-code hashes that gate a certificate, and the addresses that own
  # each panel. Both are worth the same attention, so this is "All" rather
  # than write-only: over the 30 days to 2026-09-16 these two tables consumed
  # 2 and 1 read capacity units respectively and no write capacity at all, so
  # at $0.10 per 100,000 data events the reads cost nothing to log.
  # security-alarms.tf section 14 pages on any of these events not made by the
  # scoreboard-api or scoreboard-enroll role.
  event_selector {
    read_write_type           = "All"
    include_management_events = false
    data_resource {
      type   = "AWS::DynamoDB::Table"
      values = [for t in data.aws_dynamodb_table.scoreboard_state : t.arn]
    }
  }
```

- [ ] **Step 2: Add section 14**

At the end of `terraform/security-alarms.tf`:

```hcl
# ---- 14. The scoreboard's state tables ----
#
# Sections 10 to 13 watch the control plane around the scoreboard's data: who
# may sign in, what a token is worth, who may call the functions, and who may
# widen their roles. This watches the rows.
#
#   scoreboard-devices      which account owns which panel. A write here hands
#                           somebody a panel with no API call, no token and no
#                           configuration change.
#   scoreboard-enrollments  the hashes of the collection tokens and claim codes
#                           that turn a fresh panel into a device with a
#                           certificate. A read here is enough to matter.
#
# Only two identities have a reason to touch them: the scoreboard-api role
# reads and updates devices, and the scoreboard-enroll role reads and writes
# both. Their calls arrive as userIdentity.type AssumedRole with
# sessionContext.sessionIssuer.userName equal to the role name -- stable
# across cold starts, unlike the session name. So the rule allows those two
# and pages on everything else, including an IAM user, whose events carry no
# sessionContext at all and need their own branch, exactly as section 12's
# missing invokedBy does.
#
# The trail logs these as data events, reads included (cloudtrail.tf). No
# event names are listed, so GetItem, Query, Scan, PutItem, UpdateItem,
# DeleteItem, BatchWriteItem, TransactWriteItems and ExecuteStatement are
# covered alike. eventCategory is Data, so management writes to the tables
# stay with whatever already covers them.
#
# Expected noise: none. Nothing but the two functions touches these tables
# today, and the 30 days to 2026-09-16 recorded 3 read capacity units across
# both and no writes. The owner's own scan during a recovery does page, which
# is correct: the alert names the caller, and the sentence says to check the
# owners.
#
# What it does not see:
#   - The two functions' own access. A compromised scoreboard-enroll role
#     reads and writes these tables exactly as it should. Section 13 pages
#     when that role is widened, section 12 when the function is invoked
#     directly.
#   - A stream or a restored backup. Neither table has a stream today, and
#     RestoreTableFromBackup is a management event naming a new table.
#   - A call that names a table only through a field not listed above, the
#     silent-failure mode sections 10 to 13 share.
#   - Rewriting this rule, which section 9 catches.
locals {
  scoreboard_state_tables = sort([for t in data.aws_dynamodb_table.scoreboard_state : t.name])
  scoreboard_state_roles  = ["scoreboard-api", "scoreboard-enroll"]

  scoreboard_state_pattern = jsonencode({
    "detail-type" = ["AWS API Call via CloudTrail"]
    "detail" = {
      "eventSource"   = ["dynamodb.amazonaws.com"]
      "eventCategory" = ["Data"]
      "$or" = [
        {
          "requestParameters" = { "tableName" = local.scoreboard_state_tables }
          "userIdentity"      = { "sessionContext" = { "sessionIssuer" = { "userName" = [{ "anything-but" = local.scoreboard_state_roles }] } } }
        },
        {
          "requestParameters" = { "tableName" = local.scoreboard_state_tables }
          "userIdentity"      = { "sessionContext" = [{ "exists" = false }] }
        },
      ]
    }
  })
}

resource "aws_cloudwatch_event_rule" "scoreboard_state" {
  name          = "hockeytrack-sec-scoreboard-state"
  description   = "Any read or write of the scoreboard-devices or scoreboard-enrollments rows not made by the scoreboard-api or scoreboard-enroll role"
  event_pattern = local.scoreboard_state_pattern

  lifecycle {
    precondition {
      condition     = length(local.scoreboard_state_pattern) <= 2048
      error_message = "The scoreboard state rule's event pattern is ${length(local.scoreboard_state_pattern)} characters. EventBridge rejects patterns over 2048, and only at apply."
    }
  }
}
```

- [ ] **Step 3: Register it**

In `local.security_rules`, after the `scoreboard_support` line:

```hcl
    scoreboard_state = aws_cloudwatch_event_rule.scoreboard_state
```

In `local.security_alert_meaning`, after its `scoreboard_support` line:

```hcl
    scoreboard_state = "If this was not you, assume someone read or changed the rows that decide who owns a panel and which enrollment codes are live. Check the devices table's owner column against who should hold each panel, list IoT certificates created since, and treat every collection token and claim code in the enrollments table as known to the caller."
```

Keep `terraform fmt` alignment in both maps.

- [ ] **Step 4: Check the length and the semantics offline**

Build the rendered pattern in Python (compact separators; tables `scoreboard-devices` and `scoreboard-enrollments` sorted; roles as above) and report its length; it must be `<= 2048`. Then run `aws events test-event-pattern` (read-only) on synthetic events wrapped as:

```json
{"version":"0","id":"t","detail-type":"AWS API Call via CloudTrail","source":"aws.dynamodb","account":"989232581535","time":"2026-09-16T00:00:00Z","region":"us-east-1","resources":[],"detail":<record>}
```

with these details, and put the results in your report:

- **Must not match:** `GetItem` on `scoreboard-devices` by `{"type":"AssumedRole","sessionContext":{"sessionIssuer":{"type":"Role","userName":"scoreboard-api"}}}`; the same for `scoreboard-enroll`; a `GetItem` on `scoreboard-games` by an IAM user; a management event (`eventCategory` `Management`) on `scoreboard-devices`.
- **Must match:** `PutItem` on `scoreboard-devices` by `{"type":"IAMUser","arn":"arn:aws:iam::989232581535:user/funandgames"}` with no `sessionContext`; `Scan` on `scoreboard-enrollments` by an `AssumedRole` whose `sessionIssuer.userName` is `some-other-role`; `GetItem` on `scoreboard-devices` by an `AssumedRole` whose `sessionIssuer.userName` is `scoreboard-today`.

- [ ] **Step 5: Validate**

```bash
cd /home/jay/projects/hockeytrack
terraform -chdir=terraform fmt -check && terraform -chdir=terraform validate
```
Expected: silent `fmt` (run `terraform -chdir=terraform fmt` and re-check if not), `validate` prints "Success!". Do not run `plan`.

- [ ] **Step 6: The §4 paragraph**

In `docs/threat-model.md`, after the paragraph beginning `**Changing what the scoreboard's rules lean on pages someone.**`, add:

```markdown
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
```

- [ ] **Step 7: The §7 recovery entry**

In `docs/threat-model.md` §7, after the last numbered step of the entry beginning `**A scoreboard support alert you cannot account for.**`, add:

```markdown
**A scoreboard state alert you cannot account for.** Assume the rows that
decide who owns a panel, or the secrets that mint a certificate, are in
somebody else's hands. In us-east-1:
1. Find what they touched. The alert gives the time and the Actor ARN; the
   record carries the table, the operation and the key:
   `aws logs filter-log-events --log-group-name /aws/cloudtrail/hockeytrack-account --start-time <ms> --end-time <ms> --filter-pattern '{ ($.eventSource = "dynamodb.amazonaws.com") && ($.eventCategory = "Data") }'`.
   Start one minute before the alert's time and end at least twenty minutes
   after it: the log group stamps each record when CloudTrail delivers it, not
   when the call happened.
2. Cut the credential off, as the direct-invoke entry's step 2 describes.
3. **If `scoreboard-devices` was written:** every panel's owner is now
   suspect. `aws dynamodb scan --table-name scoreboard-devices` and compare
   each row's owner with who should hold that panel. A row whose owner you do
   not recognize means that panel is being driven by somebody else: put the
   right owner back, then treat the panel as theirs until its certificate is
   replaced, because the owner column does not control the device's identity.
4. **If `scoreboard-enrollments` was read:** treat every collection token and
   claim code it held as known. A pending enrollment can be claimed by
   whoever holds its code, so delete the pending rows and re-enroll those
   panels; a completed row's collection token still fetches the certificate
   that was minted for it, so the panels it covers need new certificates.
5. **Either way, check the certificates.** `aws iot list-certificates` gives
   each one's creation date; anything created in the window that you cannot
   account for gets revoked and detached, as the admin API entry's step 8
   describes.
```

- [ ] **Step 8: Commit**

```bash
git add terraform/cloudtrail.tf terraform/security-alarms.tf docs/threat-model.md
git commit -m "security: watch the rows in the devices and enrollments tables

Section 14, hockeytrack-sec-scoreboard-state: the trail logs both tables' data
events, reads included, and any access whose session issuer is not the
scoreboard-api or scoreboard-enroll role pages.

Co-Authored-By: <your model> <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Ckux9bzkCV2K2XAp8HkM2i"
```

---

### Task 2: Apply and prove (controller and user; no subagent)

- [ ] **Step 1: Plan**

```bash
cd /home/jay/projects/hockeytrack
tag=$(aws lambda get-function --region us-east-1 --function-name hockeytrack-poller --query Code.ImageUri --output text | sed 's/.*://')
terraform -chdir=terraform plan -input=false -no-color -var="image_tag=$tag" -out=<scratchpad>/state.tfplan
terraform -chdir=terraform show -no-color <scratchpad>/state.tfplan | grep -E "will be|must be|Plan:"
```

Expected: `Plan: 2 to add, 3 to change, 0 to destroy.` — the rule and its target added; the trail, the SNS topic policy and the DLQ queue policy changed. Read the trail's diff: the only change is the added DynamoDB selector.

- [ ] **Step 2: The user applies**

`terraform -chdir=/home/jay/projects/hockeytrack/terraform apply <scratchpad>/state.tfplan`

Expected emails: audit-tampering for `PutEventSelectors`, and the section 9 rewrite rule for `PutRule` and `PutTargets`.

- [ ] **Step 3: Breaks, after a few minutes for the selector to take effect**

The 2026-09-15 work showed a new selector does not log immediately: an invoke 40 seconds after the change was never recorded. Wait five minutes, then, with the user's go-ahead:

```bash
aws dynamodb get-item --region us-east-1 --table-name scoreboard-devices --key '{"thingName":{"S":"detection-check"}}'
aws dynamodb put-item --region us-east-1 --table-name scoreboard-devices --item '{"thingName":{"S":"detection-check"},"owner":{"S":"detection-check"}}'
aws dynamodb delete-item --region us-east-1 --table-name scoreboard-devices --key '{"thingName":{"S":"detection-check"}}'
aws dynamodb scan --region us-east-1 --table-name scoreboard-enrollments --max-items 1
```

The table is empty, so the written row is the only one and it is deleted immediately. Confirm afterwards with `aws dynamodb scan --region us-east-1 --table-name scoreboard-devices --select COUNT` that the count is 0.

Expect four emails with the section 14 sentence, each naming `user/funandgames`.

- [ ] **Step 4: Confirm the identity shape and the negative**

Read the records the breaks produced:

```bash
aws logs filter-log-events --region us-east-1 --log-group-name /aws/cloudtrail/hockeytrack-account --start-time <ms> --filter-pattern '{ ($.eventSource = "dynamodb.amazonaws.com") && ($.eventCategory = "Data") }' --output json
```

Check that the IAM user's records carry no `sessionContext`, which is the branch that matched them. Then the user signs in and loads the panel list; the `scoreboard-api` role's `Query` on `scoreboard-devices` must appear in the log group with `sessionContext.sessionIssuer.userName` = `scoreboard-api`, and the rule must not match it. If the role's records carry a different shape, the rule is watching nothing — stop and return to the design.

- [ ] **Step 5: Health and drift**

```bash
for m in MatchedEvents Invocations FailedInvocations; do
  aws cloudwatch get-metric-statistics --region us-east-1 --namespace AWS/Events --metric-name $m --dimensions Name=RuleName,Value=hockeytrack-sec-scoreboard-state --start-time <ISO> --end-time <ISO> --period 300 --statistics Sum --query 'Datapoints[].Sum'
done
aws sqs get-queue-attributes --region us-east-1 --queue-url "$(aws sqs get-queue-url --region us-east-1 --queue-name hockeytrack-security-alerts-dlq --query QueueUrl --output text)" --attribute-names ApproximateNumberOfMessages
terraform -chdir=terraform plan -input=false -var="image_tag=$tag" -detailed-exitcode
```

Expected: matches equal the breaks, no failed invocations, DLQ 0, plan exit 0.

- [ ] **Step 6: Verification record**

Append `## 6. Verification record (<date>)` to the spec with the pattern table, the applies, the breaks and their emails, the identity shapes observed, the negative, the metrics and the drift check. Commit it.

- [ ] **Step 7: Finish**

Use superpowers:finishing-a-development-branch.
