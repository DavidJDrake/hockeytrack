# Detection for the Devices and Enrollments Tables — Design

**Status:** accepted (design approved in conversation, 2026-09-16)
**Date:** 2026-09-16
**Closes:** the devices table's data plane, named as a gap in `2026-09-15-api-detection-design.md` §6, `2026-09-15-direct-invoke-design.md` §7, and sections 11 and 12 of `terraform/security-alarms.tf`.
**Repository:** HockeyTrack only. The trail's selectors, one new rule (section 14), its registry entries, and the threat model.

## 1. Purpose

`scoreboard-devices` holds which account owns which panel; `scoreboard-enrollments` holds the hashes of the collection tokens and claim codes that turn a fresh panel into a device with a certificate. Every rule so far watches the control plane around them: who may sign in, what a token is worth, who may call the functions, and who may widen those functions' roles. None watches the rows themselves.

Two things follow. Someone who can write to `scoreboard-devices` can hand themselves a panel by changing its owner, with no API call, no token and no configuration change. Someone who can read `scoreboard-enrollments` learns the hashes that gate a certificate, and someone who can read `scoreboard-devices` learns which addresses own which panels.

## 2. Facts established by investigation (2026-09-16, read-only)

1. **Both tables are idle.** `scoreboard-devices` holds 0 items and 0 bytes; no panel has been claimed yet. Over 30 days the account consumed 2 read capacity units on `scoreboard-devices` and 1 on `scoreboard-enrollments`, with no writes at all. Both are `PAY_PER_REQUEST`.
2. **Data events cost $0.10 per 100,000.** At this volume the cost rounds to zero, and even with several panels in daily use the admin API makes a handful of calls per session. Logging reads as well as writes is therefore affordable, and reads are what an exfiltration looks like.
3. **Only two identities should ever touch these tables.** `scoreboard-api` (`arn:aws:iam::989232581535:role/scoreboard-api`) reads and updates `scoreboard-devices`; `scoreboard-enroll` (`role/scoreboard-enroll`) reads and writes `scoreboard-enrollments` but only writes `scoreboard-devices` (`PutItem`/`UpdateItem`, no read). Nothing else in the account has a reason to.
4. **A role's CloudTrail identity is stable.** A Lambda's calls arrive as `userIdentity.type` `AssumedRole` with `sessionContext.sessionIssuer.userName` equal to the role's name, so the rule can match the role rather than a session that changes on every cold start.

## 3. Design

### 3.1 The trail logs both tables

`aws_cloudtrail.account` gains a third `event_selector`:

```hcl
event_selector {
  read_write_type           = "All"
  include_management_events = false
  data_resource {
    type   = "AWS::DynamoDB::Table"
    values = [for t in data.aws_dynamodb_table.scoreboard_state : t.arn]
  }
}
```

The tables are found by name through `data "aws_dynamodb_table"` over `scoreboard-devices` and `scoreboard-enrollments`, so a renamed table fails the plan instead of logging nothing. `read_write_type = "All"` because a read of the enrollments table is as interesting as a write.

### 3.2 Section 14: `hockeytrack-sec-scoreboard-state`

```hcl
event_pattern = jsonencode({
  "detail-type" = ["AWS API Call via CloudTrail"]
  "detail" = {
    "eventSource"   = ["dynamodb.amazonaws.com"]
    "eventCategory" = ["Data"]
    "$or" = [
      { "userIdentity" = { "sessionContext" = { "sessionIssuer" = { "arn" = [{ "anything-but" = local.scoreboard_state_role_arns }] } } } },
      { "userIdentity" = { "sessionContext" = { "sessionIssuer" = { "arn" = [{ "exists" = false }] } } } },
    ]
  }
})
```

Two changes from the first draft, both found in review and verified live with `aws events test-event-pattern`:

- **No `requestParameters.tableName` clause.** The first draft had one in both branches, matching `GetItem`/`Query`/`Scan`/`PutItem`/`UpdateItem`/`DeleteItem` — but not `BatchGetItem`/`BatchWriteItem` (name the table inside `requestItems`), not `TransactWriteItems` (inside `transactItems[].put.tableName`), not `ExecuteStatement` (inside a `statement` string), and not any of them naming the table by ARN instead of by name. All five were confirmed silent against the drafted pattern. The fix is subtraction, not addition: `aws_cloudtrail.account` carries exactly one `AWS::DynamoDB::Table` selector, and it names exactly these two tables, so every DynamoDB data event EventBridge can deliver already belongs to one of them — the selector, not this rule, is what scopes the tables. Cost: if that selector is ever widened to a third table, this rule starts paging on that table's ordinary traffic until it is updated to match, which is the loud direction to be wrong in.
- **`sessionIssuer.arn`, not `sessionIssuer.userName`.** A role named `scoreboard-api` in some other account is a different principal with no reason to be exempt, and only the ARN says which account issued the session. The two ARNs come from `data.aws_iam_role.scoreboard["scoreboard-api"].arn` and `["scoreboard-enroll"].arn` — the same data source section 13 already declares — not literals, so a recreated role still resolves and a renamed one fails the plan.

- **Who is allowed:** `scoreboard-api` and `scoreboard-enroll`, matched by the role's ARN. Everything else pages, including an IAM user, which carries no `sessionContext.sessionIssuer` at all and needs the second branch — the same shape section 12 uses for `invokedBy`, though here `exists:false` is checked against the leaf `sessionIssuer.arn` rather than the object-valued `sessionContext`, because EventBridge's `exists` test does not reliably see presence or absence of an object-valued field, only a leaf's — also verified live.
- **No event names,** so `GetItem`, `Query`, `Scan`, `PutItem`, `UpdateItem`, `DeleteItem`, `BatchGetItem`, `BatchWriteItem`, `TransactWriteItems` and `ExecuteStatement` are covered alike, however each names the table.
- **`eventCategory` is `Data`,** so management writes to the tables keep going to whatever rule already covers them, and this rule sees only row access.
- **Expected noise: none.** Nothing but the two functions touches these tables today. The owner's own `aws dynamodb scan` during a recovery will page, which is correct: the alert names them, and the sentence says as much.
- **Length precondition** `<= 2048`, like sections 9 to 13. The shipped pattern is 395 characters.

Alert sentence: *If this was not you, assume someone read or changed the rows that decide who owns a panel and which enrollment codes are live. Check the devices table's owner column against who should hold each panel, list IoT certificates created since, and treat every collection token and claim code in the enrollments table as known to the caller.*

### 3.3 What it does not see

- **The two functions' own access.** A compromised `scoreboard-enroll` role reads and writes these tables exactly as it should; section 13 pages when that role is widened, and section 12 when the function is invoked directly.
- **DynamoDB Streams or a backup restored elsewhere.** Neither table has a stream today; `RestoreTableFromBackup` is a management event on a new table name.
- **Rewriting this rule,** which section 9 catches.

## 4. Testing

- **Before applying:** the pattern's length, and `test-event-pattern` against synthetic events built from the real shapes — an `AssumedRole` call whose `sessionIssuer.arn` is the `scoreboard-api` role's ARN (must not match), the same for `scoreboard-enroll` (must not match), an `IAMUser` call with no `sessionContext` (must match), an `AssumedRole` call from another role (must match), a role of the same name in a different account (must match, since the ARN differs), a management event (must not match), and the batch, transaction, PartiQL and by-ARN shapes that a `requestParameters.tableName` clause would have missed (must match).
- **After applying:** real records are captured from the trail's log group to confirm the identity shape before the rule is trusted, exactly as the direct-invoke work did.
- **Breaks:** `aws dynamodb get-item` and `aws dynamodb put-item` on `scoreboard-devices` as `funandgames`, with the written row deleted afterwards. This proves EventBridge delivery only if it is judged on the alert actually arriving — finding the matching record in the CloudTrail log group proves the trail logged the call, not that the rule fired or the topic published, since the rule and the log group are two independent consumers of the same trail.
- **Negatives:** ordinary admin-site use (sign in, list panels) produces no alert.
- **After:** metrics, DLQ depth, drift check, and a verification record in this spec.

## 5. The threat model

- **§4** gains a paragraph: the rows themselves are now watched, and what that does not cover.
- **§7** gains a recovery entry: from the record, identify the caller and what it read or wrote; compare the devices table's owners against who should hold each panel; revoke certificates issued since; treat every hash in the enrollments table as known, which means rotating the affected panels' collection tokens by re-enrolling them.
