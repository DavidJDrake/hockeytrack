# Security alarms (HOC-56).
#
# HOC-52 built the trail; this reads it. A log nobody looks at converts an
# attack from invisible to merely unnoticed, which is not much of an upgrade.
#
# The design goal is not prevention. It is that every route to destroying the
# archive passes through at least one event that pages someone while the
# recovery window is still open. Working through the paths:
#
#   Delete objects with the static key      -> denied by the bucket policy
#                                              without MFA, so the attacker
#                                              must first enroll one, which is
#                                              the identity rule.
#   Remove the bucket policy, then delete   -> also needs MFA, so the identity
#                                              rule again, and
#                                              DeleteBucketPolicy itself is the
#                                              archive rule.
#   Take over root, then act as root        -> account-contact rule, then the
#                                              root sign-in alarm.
#   Blind the root sign-in alarm first      -> audit log group rule: its metric
#                                              filter lives on the trail group.
#   Turn the trail off first                -> audit rule.
#   Delete these rules first                -> alerting rule.
#   Overwrite objects and wait for the      -> NOT caught by any rule here.
#   lifecycle rule to expire the originals     Bounded by s3.tf's
#                                              newer_noncurrent_versions: it
#                                              needs six overwrites of every
#                                              object AND ninety days, because
#                                              the two lifecycle conditions are
#                                              ANDed.
#
# That last row is the honest gap, and the size alarm below is a weaker net for
# it than it first appears: versioned overwrites make the bucket grow before it
# shrinks, so the collapse only shows once the originals actually expire.
# Object Lock (HOC-55) is what closes this properly.
#
# These rules sit on the DEFAULT event bus, not the hockeytrack bus: CloudTrail
# delivers "AWS API Call via CloudTrail" events there and nowhere else. IAM and
# Account are genuinely global services that record only into us-east-1, and
# the buckets here are us-east-1, so a single-region rule is correct for all
# three. Console sign-in is the exception, and is handled separately below.

# A separate topic from hockeytrack-alerts, deliberately, though not for the
# reason it first appears: hockeytrack-alerts already has a replaced policy
# (ecr-alerts.tf), so there is no fragile AWS default left to preserve. The
# real reasons are routing and blast radius. A credential alert at 3 a.m. and a
# DLQ-depth alert want different destinations and different urgency, and an
# edit to one topic's policy should not be able to silence the other.
resource "aws_sns_topic" "security" {
  name = "hockeytrack-security-alerts"
}

resource "aws_sns_topic_subscription" "security_email" {
  count     = var.alert_email == "" ? 0 : 1
  topic_arn = aws_sns_topic.security.arn
  protocol  = "email"
  endpoint  = var.alert_email
}

# Same shape as aws_sns_topic_policy.alerts in ecr-alerts.tf, which is known to
# work in this account: the account-owner statement is what CloudWatch alarms
# publish under, so cloudwatch.amazonaws.com needs no principal of its own, and
# EventBridge is granted by ArnEquals on aws:SourceArn.
#
# aws:SourceArn rather than aws:SourceAccount is a deliberate choice between two
# defensible options. AWS documents that EventBridge sets SourceAccount when
# publishing to SNS, so both would probably work. But every EventBridge-to-SNS
# policy already deployed in this account uses SourceArn, and AWS's own
# EventBridge-to-SNS examples use SourceArn or no condition at all. When the
# failure mode is an alert that never arrives, the form with three working
# precedents in the same account beats the form that merely ought to work.
#
# The condition is not decoration: this repo is public, so the topic ARN is
# discoverable, and an unconditioned service principal would let any account's
# EventBridge rule publish into these alerts and bury the real signal.
data "aws_iam_policy_document" "security_topic" {
  statement {
    sid    = "AccountOwner"
    effect = "Allow"
    principals {
      type        = "AWS"
      identifiers = ["*"]
    }
    actions = [
      "SNS:GetTopicAttributes", "SNS:SetTopicAttributes", "SNS:AddPermission",
      "SNS:RemovePermission", "SNS:DeleteTopic", "SNS:Subscribe",
      "SNS:ListSubscriptionsByTopic", "SNS:Publish",
    ]
    resources = [aws_sns_topic.security.arn]
    condition {
      test     = "StringEquals"
      variable = "AWS:SourceOwner"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }

  statement {
    sid    = "SecurityRulesPublish"
    effect = "Allow"
    principals {
      type        = "Service"
      identifiers = ["events.amazonaws.com"]
    }
    actions   = ["SNS:Publish"]
    resources = [aws_sns_topic.security.arn]
    condition {
      test     = "ArnEquals"
      variable = "aws:SourceArn"
      values   = [for r in local.security_rules : r.arn]
    }
  }
}

resource "aws_sns_topic_policy" "security" {
  arn    = aws_sns_topic.security.arn
  policy = data.aws_iam_policy_document.security_topic.json
}

locals {
  security_rules = {
    identity           = aws_cloudwatch_event_rule.identity_escalation
    audit              = aws_cloudwatch_event_rule.audit_tampering
    archive            = aws_cloudwatch_event_rule.archive_tampering
    alerting           = aws_cloudwatch_event_rule.alerting_tampering
    alerting_modify    = aws_cloudwatch_event_rule.alerting_modification
    iot                = aws_cloudwatch_event_rule.iot_tampering
    logs               = aws_cloudwatch_event_rule.audit_log_tampering
    scoreboard_signin  = aws_cloudwatch_event_rule.scoreboard_signin
    scoreboard_api     = aws_cloudwatch_event_rule.scoreboard_api
    scoreboard_invoke  = aws_cloudwatch_event_rule.scoreboard_invoke
    scoreboard_support = aws_cloudwatch_event_rule.scoreboard_support
    scoreboard_state   = aws_cloudwatch_event_rule.scoreboard_state
    scoreboard_image   = aws_cloudwatch_event_rule.scoreboard_image
  }

  # Raw CloudTrail JSON is unreadable on a phone, so the alert is rendered as a
  # sentence. A path that is absent from a given event does not drop the event:
  # AWS documents that "if you specify a variable to match a JSON path that
  # doesn't exist in the event, that variable isn't created and won't appear in
  # the output". The target is still invoked and the placeholder resolves to
  # nothing. That matters because userIdentity.arn genuinely is absent on some
  # matched events -- a failed root sign-in carries no ARN -- which is why every
  # value below is introduced by its own label: a missing one then reads as an
  # empty "Actor:" line rather than silently shifting the sentence's meaning.
  #
  # userAgent is deliberately NOT interpolated. EventBridge does not escape the
  # values it extracts, and userAgent is attacker-controlled, so a crafted quote
  # could break the template and suppress the very alert that matters.
  security_alert_transform = {
    account = "$.account"
    region  = "$.region"
    time    = "$.time"
    event   = "$.detail.eventName"
    actor   = "$.detail.userIdentity.arn"
    ip      = "$.detail.sourceIPAddress"
  }

  # The one sentence that says what an event means differs by rule, so it is
  # keyed like security_rules. Indexed rather than looked up with a default: a
  # new rule without a sentence of its own fails the plan instead of inheriting
  # one that is not true of it. That inheritance is how the IoT rule was first
  # drafted, and it would have sent someone chasing an MFA bypass at 3 a.m.
  # when the thing to check was the device policy. Plain text only: each
  # sentence lands inside a JSON string in the template below.
  archive_alert_meaning = "If this was not you, assume the archive's MFA gate is bypassed."
  security_alert_meaning = {
    identity           = local.archive_alert_meaning
    audit              = local.archive_alert_meaning
    archive            = local.archive_alert_meaning
    alerting           = local.archive_alert_meaning
    alerting_modify    = "If this was not you, assume a security alarm has been reconfigured rather than removed, which is the quieter way to silence it. Check the pattern and targets of every hockeytrack-sec rule, the security topic's policy and its subscription list, and the threshold, actions, actions-enabled flag and state of every hockeytrack-security and scoreboard alarm, against this repository."
    iot                = "If this was not you, assume an AWS credential is compromised, and check the scoreboard's device policy, certificates and IoT logging."
    logs               = "If this was not you, assume audit history has been destroyed, shortened or redirected. Check that both audit log groups still exist with 90-day retention, that the root sign-in metric filter is intact, and whether a subscription filter, KMS key or account-level log policy has appeared."
    scoreboard_signin  = "If this was not you, assume the scoreboard admin site's sign-in gate may be bypassed. Check the invite list, the user pool's triggers, app clients, identity providers and users, and the authgate function's code and environment, against the scoreboard repository."
    scoreboard_api     = "If this was not you, assume the scoreboard admin API may accept tokens or requests it should not. Check its JWT authorizer's issuer and audience, its routes' authorizers and integrations, the scoreboard-api and scoreboard-enroll functions' USER_POOL_ID and APP_CLIENT_ID environment variables, and their code, configuration, role and permissions, against the scoreboard repository."
    scoreboard_invoke  = "If this was not you, assume someone with credentials in this account called a scoreboard admin function directly, skipping API Gateway or Cognito. Find the caller and access key in the CloudTrail record, check what the function did in its logs at that time, revoke the key, then check the admin API and sign-in gate against the scoreboard repository."
    scoreboard_support = "If this was not you, assume the scoreboard's supporting resources have been changed: a function's role, the log groups its alarms and recovery steps read, the site's bucket or distribution, or a bulk export, backup or restore point on scoreboard-devices or scoreboard-enrollments. Check the enroll role's IoT permissions, the metric filters and retention on every scoreboard log group, the site bucket's policy and the distribution's origins and behaviors, and where any export or backup landed, against the scoreboard repository."
    scoreboard_state   = "If this was not you, assume someone read or changed the rows that decide who owns a panel and which enrollment codes are live. Check the devices table's owner column against who should hold each panel, list IoT certificates created since, and treat every claim code in the enrollments table as recoverable from its hash by the caller."
    scoreboard_image   = "If this was not you, assume the next panel flashed or updated would run somebody else's code. Treat every image on the mirror as suspect until its checksum matches the GitHub release it claims to come from, and do not flash a panel until it does. Check latest.json, the objects under images/, the bucket's versions for an overwrite, the distribution's origin and aliases, and the publisher role's trust and permissions policies, against the scoreboard repository."
  }

  security_alert_template = {
    for k, meaning in local.security_alert_meaning :
    k => "\"HOCKEYTRACK SECURITY: <event> in account <account> (<region>) at <time>.\\nActor: <actor>\\nSource IP: <ip>\\n\\n${meaning} Recovery procedure: docs/threat-model.md, section 7.\""
  }
}

# ---- 1. Identity and account escalation ----
#
# The load-bearing rule. The bucket policy in s3.tf is gated on
# aws:MultiFactorAuthPresent, and AdministratorAccess includes the IAM calls
# that grant MFA, so the holder of the static key can satisfy that condition in
# about five calls. Those calls are listed here. If EnableMFADevice fires and it
# was not you, the archive protection is already gone.
#
# The federated path is listed too, and is easy to miss: standing up a SAML or
# OIDC provider and assuming a role through it yields a session where
# MultiFactorAuthPresent is true from the assertion, with no MFA device ever
# enrolled. Nothing in this repo creates identity providers, so these are free
# to watch.
#
# Role-policy churn is deliberately excluded. PutRolePolicy and AttachRolePolicy
# fire on ordinary terraform applies here, and an alarm that cries wolf during
# routine work is worse than no alarm. So is UpdateAssumeRolePolicy, which
# changed legitimately during the September scheduler fix. Those are
# lateral-movement signals rather than routes to the archive, which is what this
# file defends.
resource "aws_cloudwatch_event_rule" "identity_escalation" {
  name        = "hockeytrack-sec-identity-escalation"
  description = "IAM credential or MFA change, a new identity provider, or an account contact change: the routes to bypassing the archive's MFA gate"
  event_pattern = jsonencode({
    "source"      = ["aws.iam", "aws.account"]
    "detail-type" = ["AWS API Call via CloudTrail"]
    "detail" = {
      "eventName" = [
        # Satisfying the MFA condition the archive policy depends on.
        "CreateVirtualMFADevice", "EnableMFADevice",
        "DeactivateMFADevice", "DeleteVirtualMFADevice",
        # The same thing by way of federation, no device required.
        "CreateSAMLProvider", "UpdateSAMLProvider",
        "CreateOpenIDConnectProvider",
        # New credentials, i.e. persistence.
        "CreateAccessKey", "UpdateAccessKey", "DeleteAccessKey",
        "CreateUser", "DeleteUser",
        "CreateLoginProfile", "UpdateLoginProfile",
        "AttachUserPolicy", "PutUserPolicy",
        # Redirecting the root email is the first move of a root takeover, and
        # root is the principal S3 exempts from a bucket policy's deny.
        "StartPrimaryEmailUpdate", "AcceptPrimaryEmailUpdate",
        "PutContactInformation", "PutAlternateContact",
      ]
    }
  })
}

# ---- 2. Audit tampering ----
#
# Turning off the recorder is the standard first move before doing something
# worth recording. This rule is why it is not free.
resource "aws_cloudwatch_event_rule" "audit_tampering" {
  name        = "hockeytrack-sec-audit-tampering"
  description = "CloudTrail being stopped, deleted or reconfigured"
  event_pattern = jsonencode({
    "source"      = ["aws.cloudtrail"]
    "detail-type" = ["AWS API Call via CloudTrail"]
    "detail" = {
      "eventName" = [
        "StopLogging", "DeleteTrail", "UpdateTrail",
        "PutEventSelectors", "DeleteEventDataStore",
      ]
    }
  })
}

# ---- 3. Archive and log bucket protection changes ----
#
# Scoped to the two buckets that matter rather than all of S3, so ordinary site
# deploys stay quiet.
resource "aws_cloudwatch_event_rule" "archive_tampering" {
  name        = "hockeytrack-sec-archive-tampering"
  description = "A protection on the archive or the CloudTrail log bucket being changed or removed"
  event_pattern = jsonencode({
    "source"      = ["aws.s3"]
    "detail-type" = ["AWS API Call via CloudTrail"]
    "detail" = {
      # These are CloudTrail eventName values, which are NOT always the API
      # action name and are NOT the IAM permission name. AWS: "In some cases,
      # the CloudTrail event name differs from the API action name. For example,
      # PutBucketLifecycleConfiguration is PutBucketLifecycle." Three names here
      # were wrong on the first draft and would have produced a rule that
      # silently never matched -- including the lifecycle one, which s3.tf calls
      # the load-bearing half of the archive's defence. Check any addition
      # against the bucket-level list in the S3 CloudTrail events documentation
      # rather than against the API or the IAM action, and note the trap runs
      # both ways: PutBucketPublicAccessBlock really is the event name even
      # though the API is PutPublicAccessBlock.
      "eventName" = [
        "PutBucketPolicy", "DeleteBucketPolicy", "DeleteBucket",
        "PutBucketVersioning", "PutBucketLifecycle",
        "DeleteBucketLifecycle", "PutBucketAcl",
        "PutBucketOwnershipControls", "DeleteBucketOwnershipControls",
        "PutBucketEncryption", "DeleteBucketEncryption",
        "PutBucketPublicAccessBlock", "DeleteBucketPublicAccessBlock",
        "PutBucketObjectLockConfiguration", "PutBucketRequestPayment",
        "PutBucketReplication", "DeleteBucketReplication",
      ]
      "requestParameters" = {
        "bucketName" = [aws_s3_bucket.raw.bucket, aws_s3_bucket.trail.bucket]
      }
    }
  })
}

# ---- 4. The alarming itself ----
#
# Everything above is worth about as many API calls to an admin credential as
# the MFA gate it watches. Delete four rules and a topic and the archive can be
# taken apart in silence. Destructive verbs only: terraform uses PutRule,
# PutTargets and PutMetricAlarm on every apply, and those are excluded so this
# stays quiet during ordinary work.
#
# Modification is the other half of this, and section 9 below carries it: every
# alarm listed here can be silenced by rewriting it as well as by deleting it.
#
# The irreducible residual: deleting THIS rule is itself unalarmed. Closing that
# needs a second account, which is the HOC-55 conversation.
#
# "aws.monitoring", not "aws.cloudwatch". CloudWatch answers to two different
# source values depending on how the event was delivered, and the original
# "aws.cloudwatch" here was the wrong one of the pair, so DeleteAlarms and
# DisableAlarmActions have never been able to match: no event carries both
# source "aws.cloudwatch" and detail-type "AWS API Call via CloudTrail".
# EventBridge's service reference is explicit that a CloudTrail-delivered
# CloudWatch event has source "aws.monitoring" with eventSource
# "monitoring.amazonaws.com", and that source "aws.cloudwatch" belongs to the
# native "CloudWatch Alarm State Change" and "CloudWatch Alarm Configuration
# Change" events, which are a separate delivery path with no userIdentity in
# them. Both are listed because the correction is documented rather than
# observed -- no delivered event was available to confirm it against, and an
# extra source value costs nothing but a wrong one costs the whole branch.
resource "aws_cloudwatch_event_rule" "alerting_tampering" {
  name        = "hockeytrack-sec-alerting-tampering"
  description = "The security alarming itself being removed or disabled"
  event_pattern = jsonencode({
    "source"      = ["aws.events", "aws.sns", "aws.monitoring", "aws.cloudwatch"]
    "detail-type" = ["AWS API Call via CloudTrail"]
    "detail" = {
      "eventName" = [
        "DeleteRule", "RemoveTargets", "DisableRule",
        "DeleteTopic", "Unsubscribe", "RemovePermission",
        "DeleteAlarms", "DisableAlarmActions",
      ]
    }
  })
}

# A failed publish is a dropped security alert, which is the failure this whole
# file exists to prevent. Same treatment as the ECR target in ecr-alerts.tf:
# undeliverable events land in a queue whose depth alarm (alarms.tf) notifies
# the operational topic, so a broken security channel reports itself through a
# different channel.
resource "aws_cloudwatch_event_target" "security" {
  for_each  = local.security_rules
  rule      = each.value.name
  target_id = "security-alerts"
  arn       = aws_sns_topic.security.arn

  dead_letter_config {
    arn = aws_sqs_queue.dlq["security-alerts"].arn
  }

  input_transformer {
    input_paths    = local.security_alert_transform
    input_template = local.security_alert_template[each.key]
  }
}

resource "aws_sqs_queue_policy" "security_dlq" {
  queue_url = aws_sqs_queue.dlq["security-alerts"].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "SecurityRulesDlq"
      Effect    = "Allow"
      Principal = { Service = "events.amazonaws.com" }
      Action    = "sqs:SendMessage"
      Resource  = aws_sqs_queue.dlq["security-alerts"].arn
      Condition = {
        ArnEquals = { "aws:SourceArn" = [for r in local.security_rules : r.arn] }
      }
    }]
  })
}

# ---- 5. Root console sign-in ----
#
# Not an EventBridge rule, because console sign-in is not global. CloudTrail
# records it in the region behind the sign-in endpoint, and this account has
# root logins in us-east-2 as well as us-east-1 -- a us-east-1 rule would have
# missed one of the seven root logins in the last ninety days. The multi-region
# trail now feeds one us-east-1 log group (cloudtrail.tf), so a metric filter
# there sees every region without needing a rule per region.
#
# This will page on legitimate root logins. That is intended, not a tuning
# problem: root is the one principal that can always remove the archive's bucket
# policy, and roughly one login every two weeks is a rate worth reading.
resource "aws_cloudwatch_log_metric_filter" "root_signin" {
  name           = "hockeytrack-sec-root-console-signin"
  log_group_name = aws_cloudwatch_log_group.trail.name
  pattern        = "{ ($.eventName = \"ConsoleLogin\") && ($.userIdentity.type = \"Root\") }"

  metric_transformation {
    name          = "RootConsoleSignIn"
    namespace     = "HockeyTrack/Security"
    value         = "1"
    default_value = "0"
  }
}

resource "aws_cloudwatch_metric_alarm" "root_signin" {
  alarm_name          = "hockeytrack-security-root-console-signin"
  alarm_description   = "Root console sign-in. Expected when you sign in as root; investigate immediately otherwise, because root can remove the archive's bucket policy. Recovery procedure: docs/threat-model.md, section 7."
  namespace           = "HockeyTrack/Security"
  metric_name         = "RootConsoleSignIn"
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.security.arn]
}

# ---- The trail going quiet ----
#
# Every other control here assumes the trail is running. Nothing noticed if it
# stopped, and this file's own hardening makes that worse: the log bucket's
# policy now denies the calls that maintain it, so a bad policy edit could halt
# delivery while IsLogging still reads true. StopLogging and DeleteTrail are
# already matched by the audit rule; this covers the silent modes those miss.
#
# Measured before choosing the threshold: this group takes 11 to 31 events an
# hour and was never empty across eight hours, so two consecutive empty hours
# means something is wrong rather than merely quiet. Missing data counts as
# breaching for the same reason it does on the sweeper alarm -- silence is the
# signal, so treating absence as healthy would defeat the alarm.
resource "aws_cloudwatch_metric_alarm" "trail_silent" {
  alarm_name          = "hockeytrack-security-trail-silent"
  alarm_description   = "No CloudTrail events delivered to CloudWatch Logs for two hours. The audit trail may have stopped. Check `aws cloudtrail get-trail-status --name hockeytrack-account` for LatestDeliveryError; see docs/threat-model.md, section 7."
  namespace           = "AWS/Logs"
  metric_name         = "IncomingLogEvents"
  dimensions          = { LogGroupName = aws_cloudwatch_log_group.trail.name }
  statistic           = "Sum"
  period              = 3600
  evaluation_periods  = 2
  datapoints_to_alarm = 2
  threshold           = 1
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "breaching"
  alarm_actions       = [aws_sns_topic.security.arn]
  ok_actions          = [aws_sns_topic.security.arn]
}

# ---- 6. The archive shrinking ----
#
# The one destruction path the rules above cannot see is overwriting objects,
# which is an allowed s3:PutObject and produces no denied call anywhere. What
# would notice is the archive getting smaller. S3 publishes BucketSizeBytes
# daily at no cost.
#
# A floor rather than a percentage change, because the archive only ever grows
# and a floor needs no baseline. Two honest limits. The metric is daily and
# frequently a day or two late, so this detects a catastrophe within days rather
# than hours, which is acceptable only because the recovery window is ninety
# days. And a versioned overwrite makes the bucket grow before it shrinks, so
# this sees that particular attack late rather than early.
#
# The default is anchored to a single day of a brand-new backfill: 11.15 GB
# across 222,636 objects on 2026-09-05. Re-baseline once growth is steady.
variable "archive_size_floor_bytes" {
  description = "Alarm if the raw archive drops below this many bytes. Sits under the current size; raise it as the archive grows."
  type        = number
  default     = 9000000000
  validation {
    condition     = var.archive_size_floor_bytes > 0
    error_message = "archive_size_floor_bytes must be positive; a floor of zero can never fire."
  }
}

resource "aws_cloudwatch_metric_alarm" "archive_shrank" {
  alarm_name          = "hockeytrack-security-archive-shrank"
  alarm_description   = "The raw archive is smaller than expected. Objects have been deleted or overwritten in bulk. Check S3 for recoverable noncurrent versions before anything else; see docs/threat-model.md, section 7."
  namespace           = "AWS/S3"
  metric_name         = "BucketSizeBytes"
  dimensions          = { BucketName = aws_s3_bucket.raw.bucket, StorageType = "StandardStorage" }
  statistic           = "Average"
  period              = 86400
  evaluation_periods  = 1
  threshold           = var.archive_size_floor_bytes
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.security.arn]
  ok_actions          = [aws_sns_topic.security.arn]
}

# ---- 7. The IoT control plane (SCO-16) ----
#
# The scoreboard's strongest structural claim is that devices publish nothing,
# and that claim is exactly one IoT policy deep. scoreboard-device grants
# Connect, Subscribe and Receive; a CreatePolicyVersion with setAsDefault
# rewrites it in one call, and every panel inherits the change on its next
# connect. Until this rule nothing watched aws.iot, so the property the threat
# model leans on hardest could be removed in silence.
#
# Why any IoT write matters here. The routes to undermining a device identity,
# and the events each has to pass through:
#
#   Widen what a certificate may do         -> a new default policy version, or
#                                              a policy attached to it,
#                                              including through the deprecated
#                                              AttachPrincipalPolicy.
#   Borrow another panel's scope            -> AttachThingPrincipal. The policy
#                                              keys on the thing name, so
#                                              binding a certificate to a
#                                              different thing hands it that
#                                              thing's config topic.
#   Turn a certificate into AWS credentials -> a role alias, which is the
#                                              bridge from a device identity
#                                              to an IAM role.
#   Mint, clone or revive an identity       -> every way a certificate enters
#                                              the account or becomes ACTIVE:
#                                              created, registered, transferred
#                                              in, or reactivated, which is the
#                                              same UpdateCertificate event as
#                                              deactivation.
#   Set up issuance that never calls an API -> a CA with auto-registration, a
#                                              certificate provider, or a
#                                              provisioning template or claim.
#                                              What they issue later arrives
#                                              over MQTT or on first connect,
#                                              where CloudTrail never looks, so
#                                              the setup call is the only record.
#   Skip certificates altogether            -> custom authorizers, and the
#                                              domain configurations that route
#                                              connections to them.
#   Blind the record of all of the above    -> IoT's own logging configuration,
#                                              v1 and v2. Any change alerts, not
#                                              just a disable: lowering the
#                                              level, or pointing it at a role
#                                              that cannot write, is the
#                                              quieter version of the same move.
#
# So the rule does not list those events. It matches every IoT write and
# excludes only reads that CloudTrail mislabels as writes, which fails loud
# where a list fails silent. A list has to spell every CloudTrail name right,
# and those are not always API names: the S3 rule above had three wrong on its
# first draft. Here a misspelling cannot hide an event, and an IoT API that AWS
# adds next year alerts the first time it is used. Of the names in the table,
# only AttachPolicy, AttachThingPrincipal and CreateKeysAndCertificate have
# been seen as real events in this account; the rest come from the IoT API
# reference. That distinction now matters only for the documentation. It no
# longer decides whether the rule fires.
#
# The cost is noise, and it was measured before choosing. Ninety days of IoT
# CloudTrail in us-east-1, 2026-06-25 to 2026-09-11, held 16 events with
# readOnly false: 9 DescribeEndpoint, 2 ListDomainConfigurations, and 5 from
# provisioning the first panel (CreatePolicy, CreateThing,
# CreateKeysAndCertificate, AttachPolicy, AttachThingPrincipal). A scoreboard
# plan stays quiet, because its only write-labelled call is DescribeEndpoint.
# What does fire is legitimate change, and that is accepted: make provision
# raises at least four alerts per panel, and more when it replaces a
# certificate; a scoreboard apply alerts whenever it changes an IoT resource;
# and narrowing calls such as DeleteCertificate and DetachPolicy alert too. All
# of it is rare and deliberate, and the person doing it is the person reading
# the alert.
#
# The two exclusions are reads that CloudTrail records with readOnly false, so
# readOnly [false] alone does not keep them out. DescribeEndpoint runs on every
# scoreboard plan, and accounts for all nine in the window.
# ListDomainConfigurations is a List call, and both occurrences were a
# read-only inventory of IoT resources on 2026-09-11. Add to this list only a
# read with evidence like that, never a write that merely happens often.
# readOnly [false] and eventSource say what delivery and source already imply:
# EventBridge hands a rule in the ENABLED state only write events. They are
# there so the pattern reads plainly, and so a read-only event can be tested
# as a negative.
#
# IoT is regional, unlike IAM. This rule watches us-east-1 because the policy,
# the certificates and the logging configuration are all there, and
# provision.sh writes the us-east-1 endpoint into every panel. IoT activity in
# any other region is invisible to it. That activity could not reach these
# panels, but it would be unwatched use of the account.
#
# What it does not see: anything on the data plane, including a message
# published to the panels' own topics; the IoT logging role losing its
# permissions, because role-policy edits are the churn the identity rule
# excludes; and the deletion of this rule, which the alerting rule covers. The
# AWSIotLogsV2 log group being deleted or shortened is section 8's job.
resource "aws_cloudwatch_event_rule" "iot_tampering" {
  name        = "hockeytrack-sec-iot-tampering"
  description = "Any write to the IoT control plane, where the scoreboard's publish-nothing device policy can be widened, a device identity minted or bypassed, or IoT logging changed"
  event_pattern = jsonencode({
    "source"      = ["aws.iot"]
    "detail-type" = ["AWS API Call via CloudTrail"]
    "detail" = {
      "eventSource" = ["iot.amazonaws.com"]
      "readOnly"    = [false]
      "eventName"   = [{ "anything-but" = ["DescribeEndpoint", "ListDomainConfigurations"] }]
    }
  })
}

# ---- 8. The log groups that hold audit evidence (HOC-60) ----
#
# Two CloudWatch Logs groups hold evidence rather than application output.
# The trail group (cloudtrail.tf) is CloudTrail's near-real-time copy: the root
# sign-in filter above reads it, and trail_silent watches it arrive.
# AWSIotLogsV2 holds IoT's authorization failures. The scoreboard stack owns
# it, so it is named here as a string rather than a reference. Until this
# rule, either could be emptied without a page. trail_silent notices the trail
# group going quiet, not its history going. Deleting AWSIotLogsV2 is quieter
# still: the IoT logging role may create the group, so IoT puts it back empty
# with never-expire retention, and logging carries on as though nothing
# happened.
#
# The routes to erasing, shortening, diverting or hiding that history, and the
# call each has to make:
#
#   Erase it                                -> DeleteLogGroup, or
#                                              DeleteLogStream one stream at a
#                                              time.
#   Shorten it                              -> PutRetentionPolicy. Expiry is
#                                              carried out by the service, so,
#                                              like the S3 lifecycle rule, it
#                                              deletes with no delete call
#                                              anyone could deny.
#   Unguard it                              -> PutLogGroupDeletionProtection
#                                              with false. Neither group has
#                                              deletion protection on today.
#   Blind what reads it                     -> Put or DeleteMetricFilter on the
#                                              trail group, which silences the
#                                              root sign-in alarm without
#                                              touching root.
#   Divert or copy it out                   -> a subscription filter, an export
#                                              task, a scheduled query, or a
#                                              delivery destination.
#   Make it unreadable                      -> AssociateKmsKey with a key that
#                                              is then deleted, or a data
#                                              protection policy that masks it.
#   Rewrite it on the way in                -> a transformer, which changes
#                                              events at ingestion.
#   Any of the above, account-wide          -> PutAccountPolicy, which names no
#                                              group at all.
#
# So, as with the IoT rule, the pattern does not list those calls. It matches
# every write that names either group and excludes one. The hard part is the
# field rather than the event name, because CloudWatch Logs names a group in
# three generations of parameter: logGroupName on the original APIs;
# logGroupIdentifier, a name OR an ARN, on newer ones such as data protection,
# transformers and deletion protection; and resourceArn on tagging and resource
# policies. Beyond those, the KMS calls take an ARN as resourceIdentifier;
# scheduled queries, anomaly detectors and saved queries take lists; and a
# delivery destination names its target log group one level down. The list
# below came from walking the input of every write operation in the service
# model shipped with aws-cli 2.33.2, not from memory. It is the one part of
# this rule that still fails silent: an API that names a group through a new
# field will not match until the field is added here.
#
# A field that can hold only a name matches the exact name; a field that can
# hold an ARN matches the group's ARN as a prefix, which covers the bare ARN and
# the ":*" that describe calls and cloudtrail.tf both append. Why a prefix, and
# what it over-matches, is explained above the rule below.
#
# Account policies are matched whatever they select. PutAccountPolicy can mask,
# divert or rewrite every group at once. Its selectionCriteria is a free-text
# expression, such as LogGroupName NOT IN [...], that a pattern cannot evaluate,
# so the only safe reading is that it reaches these two. DeleteAccountPolicy
# is included because an account policy is as likely to be a protection as an
# attack, and removing a protection is the move worth seeing.
#
# The noise was measured before deciding the exclusion. Ninety days of
# CloudWatch Logs CloudTrail in us-east-1, 2026-06-13 to 2026-09-11, held 9,547
# events, 7,665 of them writes. Ten writes named an audit group:
# CreateLogGroup, PutRetentionPolicy and PutMetricFilter when this file's trail
# group was created on 2026-09-07; CreateLogGroup and PutRetentionPolicy when
# the scoreboard created AWSIotLogsV2 on 2026-09-11; and five CreateLogStream
# calls, four by the CloudTrail delivery role in the trail group's first
# thirteen minutes and one by the IoT logging role. Plans stay quiet, because
# Terraform reads these groups with DescribeLogGroups, ListTagsForResource and
# DescribeMetricFilters, all recorded with readOnly true. Unlike IoT, no read in
# the window was mislabelled as a write, so there is no read to exclude. The
# honest caveat is that this measures creation, not steady state: the trail
# group is four days old and the IoT group under an hour. What does fire is
# legitimate change: an apply that touches either group, and a scoreboard
# destroy. That is accepted for the same reason as the IoT rule.
#
# The one exclusion is CreateLogStream. It adds an empty stream and cannot
# erase, shorten, divert or hide anything. Every occurrence in the window was
# a delivery role. And alerting on it would catch nothing: forged events do not
# need a new stream, because PutLogEvents writes into an existing one, and
# PutLogEvents is never recorded. The window held zero of them, though every
# Lambda here writes logs constantly. For the same reason, account-scoped
# resource policies (PutResourcePolicy with no resourceArn) are not matched:
# AWS restricts them to letting services create streams and put events, which
# only adds. One scoped to either group names it through resourceArn, and does
# alert.
#
# CloudWatch Logs is regional. Both groups are in us-east-1, and so is this
# rule. The multi-region trail delivers every region into the one group, so no
# other region holds a CloudWatch copy of it.
#
# What it does not see: writes INTO the groups, so forged entries do not alert;
# the delivery roles losing their permissions, which is role-policy churn that
# the identity rule excludes (trail_silent catches the trail side; nothing
# catches the IoT side, where silence is normal); and the deletion of this rule,
# which the alerting rule covers. Whether CloudTrail labels a Logs Insights
# StartQuery as a write is unmeasured, because no query has run in the window.
# If it does, a responder querying the trail group will page themselves, and
# StartQuery belongs in an anything-but list with that event as evidence.
locals {
  audit_log_group_names = [aws_cloudwatch_log_group.trail.name, "AWSIotLogsV2"]
  audit_log_group_arns = [
    for name in local.audit_log_group_names :
    { "prefix" = "arn:aws:logs:${var.region}:${data.aws_caller_identity.current.account_id}:log-group:${name}" }
  ]
  # Every group-naming field except logGroupName, with the forms it can carry.
  # The service model gives logGroupNames a pattern with no colon, so it can
  # only hold a name; logGroupIdentifier(s) take a name or an ARN; the rest take
  # ARNs.
  audit_log_group_other_fields = {
    logGroupNames       = local.audit_log_group_names
    logGroupIdentifier  = concat(local.audit_log_group_names, local.audit_log_group_arns)
    logGroupIdentifiers = concat(local.audit_log_group_names, local.audit_log_group_arns)
    logGroupArnList     = local.audit_log_group_arns
    resourceArn         = local.audit_log_group_arns
    resourceIdentifier  = local.audit_log_group_arns
  }
  audit_log_pattern = jsonencode({
    "source"      = ["aws.logs"]
    "detail-type" = ["AWS API Call via CloudTrail"]
    "detail" = {
      "eventSource" = ["logs.amazonaws.com"]
      "readOnly"    = [false]
      "$or" = concat(
        [{
          "eventName"         = [{ "anything-but" = ["CreateLogStream"] }]
          "requestParameters" = { "logGroupName" = local.audit_log_group_names }
        }],
        [for field, values in local.audit_log_group_other_fields : { "requestParameters" = { (field) = values } }],
        [
          { "requestParameters" = { "deliveryDestinationConfiguration" = { "destinationResourceArn" = local.audit_log_group_arns } } },
          { "eventName" = ["PutAccountPolicy", "DeleteAccountPolicy"] },
        ],
      )
    }
  })
}

# Three decisions in the pattern above are not style.
#
# The CreateLogStream exclusion sits on the logGroupName branch only, because
# CreateLogStream takes no other group field (its input is logGroupName and
# logStreamName, per the service model). That also keeps eventName out of the
# top level of detail, which matters: when eventName is constrained both beside
# a $or and inside one of its branches, EventBridge's answer depends on JSON key
# order. The same pattern and the same unrelated event gave false with eventName
# written first and true with $or first, and jsonencode sorts "$" before "e", so
# Terraform always renders the broken order. The first draft did exactly that
# and matched every write in the account.
#
# ARN-shaped fields match the group's ARN as a prefix, so the bare ARN, its
# ":*" form and anything after it all match. The prefix also matches a sibling
# group whose name merely begins the same way. That over-match can only raise a
# false alarm, never hide a real one, and it was accepted because EventBridge
# rejects a pattern over 2048 characters: the exact forms (name, bare ARN, and
# ARN plus ":", on every field) came to 3,942, and the first apply failed on it.
#
# That limit is enforced only at apply, after a plan has passed, so the
# precondition below moves the failure to plan.
resource "aws_cloudwatch_event_rule" "audit_log_tampering" {
  name          = "hockeytrack-sec-audit-log-tampering"
  description   = "Any write naming the CloudTrail or IoT log group, or any account-wide log policy change: the routes to deleting, shortening, diverting or hiding audit history"
  event_pattern = local.audit_log_pattern

  lifecycle {
    precondition {
      condition     = length(local.audit_log_pattern) <= 2048
      error_message = "The audit log group rule's event pattern is ${length(local.audit_log_pattern)} characters. EventBridge rejects patterns over 2048, and only at apply."
    }
  }
}

# ---- 9. Reconfiguring the alerting path, rather than dismantling it (HOC-61) ----
#
# Section 4 watches eight destructive calls. Every alarm it protects can be
# silenced just as completely by a call that modifies it, and none of those
# paged until this rule. Rewriting is also the quieter move: a deleted rule is
# missing from a plan, while a rule whose pattern no longer matches anything
# still exists, still has its target, and still reports as ENABLED.
#
# The routes to a silent alerting path, and the call each has to make:
#
#   Make a rule match nothing               -> PutRule with a narrower pattern.
#                                              The rule survives; it just never
#                                              fires again.
#   Cut a rule off from the topic           -> PutTargets, repointing the target
#                                              elsewhere or dropping the input
#                                              transformer that renders the
#                                              alert.
#   Turn a rule off without deleting it     -> DisableRule.
#   Raise an alarm out of reach             -> PutMetricAlarm with a threshold
#                                              nothing will cross, or with
#                                              alarm_actions emptied, or with
#                                              actionsEnabled false.
#   Pin an alarm to healthy                 -> SetAlarmState forcing OK.
#   Drop every alert at the topic           -> SetSubscriptionAttributes adding
#                                              a filter policy that matches
#                                              nothing, which is invisible
#                                              unless you read the subscription.
#   Stop the rules publishing               -> SetTopicAttributes rewriting the
#                                              topic policy, so the rules are
#                                              refused, or the delivery policy,
#                                              so retries stop.
#   Leak the alerts instead of hiding them  -> Subscribe, adding a subscriber
#                                              nobody asked for. This one does
#                                              not silence anything; it is here
#                                              because a security topic gaining
#                                              a reader is worth the same look.
#
# So, as with sections 7 and 8, the rule does not list those calls. It matches
# any write to EventBridge, SNS or CloudWatch that NAMES a security resource,
# whichever field carries the name. A list of event names would have to spell
# each one right and would miss whatever AWS ships next; this fails loud
# instead. It is also why DeleteRule, RemoveTargets and Unsubscribe on a
# security resource now alert twice, once here and once through section 4. A
# duplicate page on a genuinely bad event is the right side to err on.
#
# The difference from sections 7 and 8 is that matching every write outright
# would be unusable. PutRule, PutTargets and PutMetricAlarm are what ordinary
# Terraform does, in this account and two others sharing it. So this rule is
# scoped by resource, and the scoping rests on a naming convention: the
# security rules are hockeytrack-sec-*, this stack's security alarms are
# hockeytrack-security-*, and the topic is hockeytrack-security-alerts, so the
# single prefix "hockeytrack-sec" covers all three. Most of the scoreboard's
# alarms are security alarms too: its IoT authorization, enrollment, sign-in
# refusal and sign-in gate alarms publish to this topic. Two do not --
# scoreboard-iot-publish-retained-auth-error and scoreboard-dlq-depth notify
# the operational alerts topic -- but every one is named scoreboard-*, so that
# prefix is named here as a literal, the way AWSIotLogsV2 is in section 8, and
# rewriting any of them pages, those two included; that is accepted. It was
# scoreboard-iot- until 2026-09-14, which left the enrollment and sign-in alarms
# rewritable without a page. They are owned by the other repository, whose
# tests fail if an alarm there loses the prefix; a rename that dropped it would
# otherwise silently end this rule's coverage.
#
# Which field names the resource was taken from the input of every write
# operation in the events, sns and cloudwatch service models shipped with
# aws-cli 2.33.2, then confirmed against the casing CloudTrail actually records,
# which is not the model's: the models say Name, Rule, AlarmName, TopicArn, and
# CloudTrail writes name, rule, alarmName, topicArn. Both spellings of the
# tagging field are real and neither is a typo -- EventBridge and CloudWatch
# record resourceARN, SNS records resourceArn.
#
#   name             PutRule, DeleteRule, DisableRule, EnableRule, CreateTopic
#   rule             PutTargets, RemoveTargets
#   alarmName        PutMetricAlarm, PutCompositeAlarm, SetAlarmState
#   alarmNames       DeleteAlarms, DisableAlarmActions, EnableAlarmActions
#   topicArn         SetTopicAttributes, Subscribe, AddPermission,
#                    RemovePermission, DeleteTopic
#   subscriptionArn  SetSubscriptionAttributes, Unsubscribe
#   resourceArn      SNS TagResource, UntagResource, PutDataProtectionPolicy
#   resourceARN      EventBridge and CloudWatch TagResource, UntagResource
#
# Names match by prefix and ARNs match by prefix, which over-matches in two
# directions worth stating. A future resource called hockeytrack-secondary
# would alert, and so would an EventBridge event bus, connection or API
# destination named hockeytrack-sec-anything, because those share the "name"
# field with rules. Both raise a false alarm rather than hiding a real one.
#
# The noise was measured before choosing any of it, while the scoreboard prefix
# was still scoreboard-iot-. Ninety days of CloudTrail in us-east-1, to
# 2026-09-11: events.amazonaws.com held 1,688 events of which 40 were writes,
# sns.amazonaws.com 924 of which 42 were writes, and monitoring.amazonaws.com
# 1,094 of which 27 were writes. Of those 109 writes,
# 33 named a security resource and would have fired this rule: 7 PutRule and 6
# PutTargets on the hockeytrack-sec rules, 1 CreateTopic, 8 SetTopicAttributes
# and 1 Subscribe on the security topic, and 10 PutMetricAlarm, four on
# hockeytrack-security alarms and six on the scoreboard's. Every one was a
# terraform apply by the funandgames user -- this repository's, or the
# scoreboard's for its own alarms -- on 2026-09-07, 09-09 and 09-11. The other
# 76 writes were HealthTracker's and
# EbookShare's CloudFormation stacks, the hockeytrack bus and its goal
# notifications, the ECR scan rule and the scoreboard's game-events rule, and
# none of them name a security resource. Re-measured under today's
# scoreboard- prefix on 2026-09-15, over the same window as far as CloudTrail
# still held it (2026-06-20 to 2026-09-11, the same 109 writes): 34 match, the
# extra one a PutMetricAlarm on scoreboard-dlq-depth on 2026-09-07. The
# enrollment and sign-in refusal alarms were created after the window closed
# but before the widening, so their creation paged nobody. The two sign-in gate
# alarms were created after it, and each creation paged, as intended.
#
# Of the modify calls this rule exists for -- DisableRule, EnableRule,
# SetAlarmState, DeleteAlarms, DisableAlarmActions, SetSubscriptionAttributes,
# AddPermission, RemovePermission, PutCompositeAlarm, PutDataProtectionPolicy
# -- the window held not one occurrence, on any resource. Watching them is free.
# What is not free is that an apply touching a security resource now pages, at
# least once for every alarm it rewrites. "Up to ten times" was the figure here
# under the old prefix. The alarms in scope are now four of this repository's
# (the three in this file and hockeytrack-security-alerts-dlq-depth, from
# alarms.tf) and all thirteen of the scoreboard's, so a scoreboard apply that
# rewrote every alarm it owns would page at least thirteen times. Accepted on
# the same terms as sections 7 and 8: it is rare, deliberate, and the person
# doing it is the person reading the alert. Plans and no-op applies stay
# silent, because Terraform reads these with DescribeRule, ListTargetsByRule,
# DescribeAlarms, GetTopicAttributes and ListTagsForResource, and calls PutRule
# or PutMetricAlarm only when something actually differs -- 409 DescribeRule
# against 15 PutRule in the window.
#
# readOnly [false] is not what keeps those reads out, and it is worth being
# exact about why it is here. 677 read events in the window named a security
# resource in one of the fields above: 125 DescribeRule, 116 ListTagsForResource,
# 114 ListTargetsByRule, 98 DescribeAlarms, and so on. None of them can reach
# this rule, because EventBridge documents that a rule in the ENABLED state
# matches everything "except for read-only AWS management events delivered
# through CloudTrail" -- receiving those needs the state
# ENABLED_WITH_ALL_CLOUDTRAIL_MANAGEMENT_EVENTS, which nothing here sets. So
# readOnly [false] is a second lock on a door that is already shut: it costs
# nothing, it makes those 677 events testable as negatives, and it is what
# would hold if that state were ever changed.
#
# eventSource rather than source. The two say the same thing, but eventSource
# is the field this was verified against, because it appears in the CloudTrail
# records that can actually be read back. source is derived, and for CloudWatch
# it is derived to a value that is easy to get wrong -- see the note above
# section 4, where getting it wrong disabled a branch for four days.
#
# What it does not see:
#   - A call that names one of these resources only through a field not listed
#     above. This is the same silent-failure mode as section 8's field list.
#   - Rewriting THIS rule. A PutRule that narrows this pattern produces an
#     event that arrives minutes later, by which time the pattern it would be
#     matched against is already the attacker's. Deleting it is caught, by
#     section 4, which is not resource-scoped. Rewriting it is the residual,
#     and it is the second-account argument in section 5 again.
#   - Anything outside us-east-1. All of these resources are there.
#   - The alarms' data: a metric filter or metric that stops producing
#     datapoints silences an alarm without any call to CloudWatch at all.
locals {
  # "hockeytrack-sec" is a prefix of the rule names, of the topic name, and of
  # this stack's alarm names, so one entry covers all three.
  alerting_prefix         = "hockeytrack-sec"
  alerting_foreign_prefix = "scoreboard-"
  alerting_arn_stem       = "${var.region}:${data.aws_caller_identity.current.account_id}"

  alerting_names       = [{ "prefix" = local.alerting_prefix }]
  alerting_alarm_names = [{ "prefix" = local.alerting_prefix }, { "prefix" = local.alerting_foreign_prefix }]
  # A subscription ARN is the topic ARN plus ":<uuid>", so one prefix serves the
  # topic and its subscriptions both.
  alerting_topic_arns = [{ "prefix" = "arn:aws:sns:${local.alerting_arn_stem}:${aws_sns_topic.security.name}" }]
  alerting_tag_arns = [
    { "prefix" = "arn:aws:events:${local.alerting_arn_stem}:rule/${local.alerting_prefix}" },
    { "prefix" = "arn:aws:cloudwatch:${local.alerting_arn_stem}:alarm:${local.alerting_prefix}" },
    { "prefix" = "arn:aws:cloudwatch:${local.alerting_arn_stem}:alarm:${local.alerting_foreign_prefix}" },
  ]

  alerting_modify_fields = {
    name            = local.alerting_names
    rule            = local.alerting_names
    alarmName       = local.alerting_alarm_names
    alarmNames      = local.alerting_alarm_names
    topicArn        = local.alerting_topic_arns
    subscriptionArn = local.alerting_topic_arns
    resourceArn     = local.alerting_topic_arns
    resourceARN     = local.alerting_tag_arns
  }

  # eventName appears nowhere in this pattern, which is deliberate and is the
  # other half of section 8's lesson: constrain eventName both beside a $or and
  # inside one of its branches and EventBridge's verdict depends on JSON key
  # order, which jsonencode always renders the wrong way round. No field here is
  # constrained in both places -- eventSource and readOnly appear only at the
  # top level, requestParameters only inside the branches.
  alerting_modify_pattern = jsonencode({
    "detail-type" = ["AWS API Call via CloudTrail"]
    "detail" = {
      "eventSource" = ["events.amazonaws.com", "sns.amazonaws.com", "monitoring.amazonaws.com"]
      "readOnly"    = [false]
      "$or"         = [for field, values in local.alerting_modify_fields : { "requestParameters" = { (field) = values } }]
    }
  })
}

resource "aws_cloudwatch_event_rule" "alerting_modification" {
  name          = "hockeytrack-sec-alerting-modification"
  description   = "Any write naming a security rule, the security topic, one of its subscriptions, or a security alarm: the routes to silencing an alert by rewriting it rather than deleting it"
  event_pattern = local.alerting_modify_pattern

  lifecycle {
    precondition {
      condition     = length(local.alerting_modify_pattern) <= 2048
      error_message = "The alerting modification rule's event pattern is ${length(local.alerting_modify_pattern)} characters. EventBridge rejects patterns over 2048, and only at apply."
    }
  }
}

# ---- 10. The scoreboard admin site's sign-in gate ----
#
# The scoreboard's admin site signs people in with Google, and only invited
# addresses get an account. What enforces that is not IAM and not Cognito's own
# settings. It is one Lambda, scoreboard-authgate, which the user pool calls as
# its pre sign-up and pre token generation triggers, reading the invite list
# from one SSM parameter. The pool's allow_admin_create_user_only was verified
# live NOT to stop federated sign-up, so the triggers are the control. That
# makes three resources an authorization root, and until this rule a write to
# any of them paged nobody:
#
#   Drop the pool's triggers                -> UpdateUserPool. Fails OPEN:
#                                              every Google account admitted.
#   Rewrite the function, or point its      -> UpdateFunctionCode*,
#   ALLOWLIST_PARAMETER elsewhere              UpdateFunctionConfiguration*.
#   Invite yourself                         -> PutParameter.
#   Add an identity provider you control    -> CreateIdentityProvider. The gate
#                                              admits any external provider
#                                              whose mapping says the address is
#                                              verified.
#   Add or widen an app client              -> Create/UpdateUserPoolClient.
#   Rewrite a user directly                 -> AdminUpdateUserAttributes and
#                                              the other Admin* calls.
#   Stop the gate running                   -> RemovePermission*,
#                                              DeleteFunction*,
#                                              DeleteParameter(s). Fails
#                                              closed, but still unexplained.
#
# Scoped by resource, like section 9, and for the same reason as section 9's
# prefixes: this account also runs LitLibrary's and HealthTracker's user pools,
# Lambdas and parameters, so a service-wide rule would page on their work. Like
# section 9, it does not list the calls it catches, and it has no eventName
# constraint at all -- sections 7 and 8 both constrain eventName, one with an
# anything-but exclusion and the other with two literal names, but this rule
# has neither. It matches any write that names one of the three resources, in
# whichever request field names it. A misspelled CloudTrail name therefore
# cannot hide a route, and an API added later alerts the first time it is
# used, provided it names the resource in one of the fields below; one that
# names it some other way is the silent-failure mode noted at the end of this
# section.
#
# Which fields name them comes from the input shape of every operation outside
# Get/List/Describe in the cognito-idp, lambda and ssm service models shipped
# with aws-cli 2.33.2, cased the way CloudTrail records them. That casing was
# confirmed against real events on 2026-09-14 for userPoolId, functionName and
# name:
#
#   userPoolId    every Cognito configuration and admin write; 58 operations
#                 outside Get/List/Describe take it, a few of them reads such
#                 as AdminGetUser
#   resourceArn   Cognito TagResource/UntagResource (the pool's ARN), and SSM
#                 Put/DeleteResourcePolicy (the parameter's ARN)
#   functionName  every Lambda write that takes a function; 30 operations
#                 outside Get/List take it, Invoke among them. CloudTrail
#                 records it both bare and as a full ARN, so it is matched
#                 with a wildcard.
#   resource      Lambda TagResource/UntagResource, an ARN
#   name          SSM PutParameter, DeleteParameter, (Un)LabelParameterVersion
#   names         SSM DeleteParameters, a list
#   resourceId    SSM AddTagsToResource/RemoveTagsFromResource
#
# Sign-in traffic does not page. The hosted-UI events recorded for every
# sign-in (Token_POST, OAuth2Response_GET, Logout) carry no requestParameters at
# all, and InitiateAuth and SignUp carry clientId, not userPoolId. Those are
# attempts against the gate, which the scoreboard's refusal alarm counts, not
# changes to it. That leaves the gate's own Invoke, once per sign-in as the
# pre sign-up and pre token generation triggers fire. The trail logs it, for
# this function and the admin API's two (cloudtrail.tf, since 2026-09-15), and
# functionName would match it exactly like a Lambda write, so every sign-in
# would page. "eventCategory" = ["Management"] is what stops that: an Invoke
# record's category is Data. Section 12 is the rule that watches invocations,
# and pages only on one Cognito did not make. Plans stay silent not because
# readOnly [false] blocks them but because an ENABLED rule never receives
# read-only management events in the first place, exactly as section 9
# explains; readOnly [false] is a second lock on a door already shut, kept
# for the same clarity reason. CloudTrail masks
# PutParameter's value and CreateIdentityProvider's client_secret, so no
# invited address and no Google secret reaches this rule's input.
#
# What does page is legitimate change, and that is accepted: a scoreboard apply
# that touches these resources, every invitation or removal, and any admin
# action on the pool. All of it is rare and deliberate, and the person doing it
# is the person reading the alert.
#
# The pool ID is looked up, not typed, because the pool can be replaced (its
# username_attributes forces a new pool). But the lookup only runs when
# HockeyTrack itself plans, so a replacement is picked up at HockeyTrack's
# next apply, not the scoreboard's. Until then, this rule keeps watching the
# old pool ID, and nothing prompts a HockeyTrack apply to happen sooner:
# writes to the new pool in that window, including an UpdateUserPool that
# drops the triggers, page nobody. The one write that does page is the
# replacement's own DeleteUserPool on the old pool, because that ID is still
# what this rule matches. The precondition fails the plan unless exactly one
# pool has this name, so a missing or duplicated pool is refused rather than
# silently watched -- at a cost: HockeyTrack's plan then fails outright,
# blocking every other security change until the scoreboard pool situation is
# resolved. The function and the parameter have fixed names in the
# scoreboard's Terraform and are named here as literals, the way AWSIotLogsV2 is
# in section 8. If that repository renames either, this rule silently stops
# covering it.
#
# What it does not see, the most important first. The gate decides who gets a
# token; it does not decide what a token is worth. The scoreboard's admin API
# does, and it is a second authorization root: section 11 watches it, as a
# separate rule, so its alert names the authorizer and routes rather than the
# invite list.
#
# The rest: the static site's bucket and distribution, which could serve a
# look-alike sign-in page -- which section 13 now pages on; a crash or
# throttle, which are not API calls and which the scoreboard's own authgate
# alarms watch; rewriting this rule, which section 9 catches; a pool
# replacement between HockeyTrack applies, covered above; and a call that
# names one of these three resources only through a field not listed above,
# the same silent-failure mode as section 8's and section 9's field lists.
#
# Nor does it see the quieter ways to blind or close the gate, because it
# watches neither CloudWatch Logs nor IAM. Deleting or rewriting the metric
# filters on /aws/lambda/scoreboard-authgate silences the refusal and failures
# alarms, which section 13 now pages on; section 8 names only the trail group
# and AWSIotLogsV2. Taking the logs permissions off the gate's role does the
# same while the gate carries on deciding -- section 13 now pages on that too,
# scoped to these seven roles -- and taking ssm:GetParameter off it fails the
# gate closed, logging each refusal as "invite list unavailable" but paging
# only at three in an hour. Section 1 deliberately excludes role-policy churn.
# The failures filter also assumes Lambda's default text log format: switching
# the function to JSON logging changes how the runtime writes those lines,
# though that switch is itself an UpdateFunctionConfiguration, which this rule
# pages on.
data "aws_cognito_user_pools" "scoreboard" {
  name = "scoreboard-admins"
}

locals {
  scoreboard_signin_arn_stem  = "${var.region}:${data.aws_caller_identity.current.account_id}"
  scoreboard_signin_parameter = "/scoreboard/allowed-emails"

  scoreboard_signin_pattern = jsonencode({
    "detail-type" = ["AWS API Call via CloudTrail"]
    "detail" = {
      "eventSource"   = ["cognito-idp.amazonaws.com", "lambda.amazonaws.com", "ssm.amazonaws.com"]
      "eventCategory" = ["Management"]
      "readOnly"      = [false]
      "$or" = [
        { "requestParameters" = { "userPoolId" = data.aws_cognito_user_pools.scoreboard.ids } },
        { "requestParameters" = { "functionName" = [{ "wildcard" = "*scoreboard-authgate*" }] } },
        { "requestParameters" = { "resource" = [{ "wildcard" = "*:function:scoreboard-authgate*" }] } },
        { "requestParameters" = { "name" = [local.scoreboard_signin_parameter] } },
        { "requestParameters" = { "names" = [local.scoreboard_signin_parameter] } },
        { "requestParameters" = { "resourceId" = [local.scoreboard_signin_parameter] } },
        { "requestParameters" = { "resourceArn" = concat(
          data.aws_cognito_user_pools.scoreboard.arns,
          ["arn:aws:ssm:${local.scoreboard_signin_arn_stem}:parameter${local.scoreboard_signin_parameter}"],
        ) } },
      ]
    }
  })
}

resource "aws_cloudwatch_event_rule" "scoreboard_signin" {
  name          = "hockeytrack-sec-scoreboard-signin"
  description   = "Any write naming the scoreboard admin site's user pool, sign-in gate function or invite list: the routes to bypassing or blinding the gate"
  event_pattern = local.scoreboard_signin_pattern

  lifecycle {
    precondition {
      condition     = length(data.aws_cognito_user_pools.scoreboard.ids) == 1
      error_message = "Expected exactly one Cognito user pool named scoreboard-admins, found ${length(data.aws_cognito_user_pools.scoreboard.ids)}. With none, the scoreboard sign-in rule would watch no pool; with several, it cannot tell which one is the gate's."
    }
    precondition {
      condition     = length(local.scoreboard_signin_pattern) <= 2048
      error_message = "The scoreboard sign-in rule's event pattern is ${length(local.scoreboard_signin_pattern)} characters. EventBridge rejects patterns over 2048, and only at apply."
    }
  }
}

# ---- 11. The scoreboard admin API ----
#
# Section 10 watches the gate that decides who gets a token. This watches what
# decides what a token is worth. That decision now rests on two checks: API
# Gateway's JWT authorizer runs in front of the scoreboard-api and
# scoreboard-enroll functions as a first gate, and each function also
# verifies the caller's raw ID token itself -- signature, issuer, exact
# audience, token use, expiry -- against the pool and client named in its own
# USER_POOL_ID and APP_CLIENT_ID environment variables, never trusting the
# claims the authorizer hands it in the event. So the API, the two functions
# and their environment variables are together an authorization root, and a
# write to any of them can claim or control panels without going near the
# pool, the gate or the invite list:
#
#   Point the authorizer elsewhere          -> UpdateAuthorizer, CreateAuthorizer.
#                                              No longer sufficient by itself:
#                                              a token from an issuer the
#                                              attacker runs still fails the
#                                              function's own check against
#                                              USER_POOL_ID and APP_CLIENT_ID,
#                                              is refused with 401, and logs a
#                                              mismatch line the scoreboard
#                                              alarms on. It still matters
#                                              alongside a change to one of
#                                              those two variables, or if a
#                                              route is moved off the
#                                              authorizer entirely.
#   Change either function's configuration  -> UpdateFunctionConfiguration*
#                                              (including its role). This is
#                                              what now moves trust:
#                                              repointing USER_POOL_ID or
#                                              APP_CLIENT_ID makes the
#                                              function itself believe a
#                                              different issuer or client.
#   Move a route, or repoint an integration -> UpdateRoute, CreateRoute,
#                                              UpdateIntegration. Removing a
#                                              route's authorizer alone is not
#                                              enough: the function still
#                                              verifies the token itself and
#                                              returns 401 without one.
#   Change either function otherwise        -> UpdateFunctionCode*,
#                                              AddPermission*,
#                                              CreateFunctionUrlConfig.
#   Reshape or reroute the API              -> UpdateStage, UpdateApi,
#                                              CreateApiMapping, DeleteApi.
#
# Scoped by resource for the same reason as section 10: the account runs three
# other HTTP APIs (davidjdrake-api, healthtracker-api and HttpApi) and many
# other functions. It has no eventName constraint.
#
# Which fields name them comes from the apigatewayv2 and lambda service models
# shipped with aws-cli 2.33.2, cased as CloudTrail records them. Ninety days of
# real events, to 2026-09-15, confirmed apiId and resource-arn:
#
#   apiId         every write to the API and everything under it: routes,
#                 integrations, authorizers, stages, deployments, models, CORS,
#                 API mappings (35 operations outside Get/List take it)
#   resource-arn  TagResource/UntagResource, arn:aws:apigateway:<region>::/apis/<id>
#                 and anything under that ARN, so it is matched by prefix. The
#                 hyphen is the wire name, not a typo.
#   functionName  every Lambda write that takes a function; matched by wildcard,
#                 because CloudTrail records it both bare and as a full ARN
#   resource      Lambda TagResource/UntagResource, an ARN
#
# Measured before choosing, over ninety days to 2026-09-15: 1,427 API Gateway
# events, 1,340 of them reads; 5 writes to scoreboard-api and 4 to
# scoreboard-enroll, all the scoreboard's own applies. Most of those code
# updates changed no code: Go stamped each binary with its commit, so every
# commit redeployed every function, and each redeploy's UpdateFunctionCode
# write would have paged, had any rule watched these two functions then. As of
# the companion scoreboard change, its make build passes -buildvcs=false and
# -trimpath, so a function keeps its code hash, and no longer triggers that
# write, unless its code, its dependencies or the Go toolchain building it
# change: each binary still embeds the Go version and its module versions, and
# the scoreboard's go.mod names go 1.27.0 under the default GOTOOLCHAIN=auto,
# which builds with whichever Go is invoked, provided it is 1.27.0 or newer. This rule still fires on
# any write; unchanged code simply no longer causes one. Reads stay silent for
# section 9's reason: an ENABLED rule never receives read-only management
# events.
#
# The API ID is looked up by name, with a precondition that exactly one API
# has it, like section 10's pool: a replaced API is picked up at this
# repository's next apply, and until then writes to the new API page nobody,
# while the teardown of the old one -- DeleteRoute, DeleteIntegration,
# DeleteAuthorizer, DeleteStage and DeleteApi, all carrying the old apiId --
# and any write to either function still page throughout. As with section
# 10's pool, the precondition fails the plan unless exactly one API has this
# name, so a missing or duplicated API is refused rather than silently
# watched -- at a cost: HockeyTrack's plan then fails outright, blocking every
# other security change until the scoreboard API situation is resolved. The
# function names are literals, like section 10's.
#
# What it does not see, the most important first, after one thing it ignores:
#   - Invocations. API Gateway's own calls to these functions are Invoke
#     records naming them, which the trail now logs (cloudtrail.tf), so this
#     rule ignores data events by eventCategory or it would page on every
#     admin-API request. Direct invocation with forged claims is closed in two
#     other places: both handlers verify the caller's ID token themselves
#     (the scoreboard's cloud/internal/idtoken) rather than trusting the
#     event's authorizer claims, and section 12 pages on any invocation API
#     Gateway did not make.
#   - Deleting or shortening the logs the recovery steps read. A DeleteLogGroup,
#     DeleteLogStream or PutRetentionPolicy on /aws/apigateway/scoreboard-admin,
#     /aws/lambda/scoreboard-api or /aws/lambda/scoreboard-enroll used to page
#     nobody -- section 8 names only the trail group and AWSIotLogsV2 -- but
#     section 13 now pages on it. The threat model's recovery entry depends on
#     those groups. Removing access logging from the stage itself does page,
#     because UpdateStage and DeleteAccessLogSettings both name the API.
#   - A custom domain rerouted away from the API. DeleteApiMapping names only
#     the domain and the mapping, and UpdateDomainName names only the domain.
#     CreateRoutingRule and PutRoutingRule do carry the API ID, but nested at
#     actions[].invokeApi.apiId, which this rule's top-level apiId match
#     cannot see. The admin API has no custom domain today, so none of these
#     can reach it yet. Adding one means adding its domainName here.
#   - The two functions' IAM roles. HockeyTrack's identity rule excludes role-policy
#     churn deliberately, but scoreboard-enroll's role can mint device
#     certificates, so a widened grant there is a real route this does not
#     watch -- section 13 now pages on it, scoped to these seven roles.
#   - The devices table's ownership rows, which section 14 now pages on.
#   - The static site's bucket and distribution, which section 13 now pages on.
#   - A call that names these resources only through a field not listed above,
#     the silent-failure mode sections 8 to 10 share.
#   - Rewriting this rule, which section 9 catches.
data "aws_apigatewayv2_apis" "scoreboard_admin" {
  name = "scoreboard-admin"
}

locals {
  scoreboard_api_ids = tolist(data.aws_apigatewayv2_apis.scoreboard_admin.ids)

  scoreboard_api_pattern = jsonencode({
    "detail-type" = ["AWS API Call via CloudTrail"]
    "detail" = {
      "eventSource"   = ["apigateway.amazonaws.com", "lambda.amazonaws.com"]
      "eventCategory" = ["Management"]
      "readOnly"      = [false]
      "$or" = [
        { "requestParameters" = { "apiId" = local.scoreboard_api_ids } },
        { "requestParameters" = { "resource-arn" = [for id in local.scoreboard_api_ids : { "prefix" = "arn:aws:apigateway:${var.region}::/apis/${id}" }] } },
        { "requestParameters" = { "functionName" = [{ "wildcard" = "*scoreboard-api*" }, { "wildcard" = "*scoreboard-enroll*" }] } },
        { "requestParameters" = { "resource" = [{ "wildcard" = "*:function:scoreboard-api*" }, { "wildcard" = "*:function:scoreboard-enroll*" }] } },
      ]
    }
  })
}

resource "aws_cloudwatch_event_rule" "scoreboard_api" {
  name          = "hockeytrack-sec-scoreboard-api"
  description   = "Any write naming the scoreboard admin API or the scoreboard-api or scoreboard-enroll function: the routes to changing what the API believes about a caller"
  event_pattern = local.scoreboard_api_pattern

  lifecycle {
    precondition {
      condition     = length(local.scoreboard_api_ids) == 1
      error_message = "Expected exactly one API Gateway API named scoreboard-admin, found ${length(local.scoreboard_api_ids)}. The scoreboard API rule would watch the wrong API, or none."
    }
    precondition {
      condition     = length(local.scoreboard_api_pattern) <= 2048
      error_message = "The scoreboard API rule's event pattern is ${length(local.scoreboard_api_pattern)} characters. EventBridge rejects patterns over 2048, and only at apply."
    }
  }
}

# ---- 12. Direct invocation of the scoreboard's admin-path functions ----
#
# Sections 10 and 11 watch changes to the sign-in gate and the admin API. This
# watches calls. scoreboard-api and scoreboard-enroll exist to be invoked by
# API Gateway, and scoreboard-authgate by Cognito; each function's resource
# policy grants only that service. Anyone in this account whose own policy
# allows lambda:InvokeFunction can still invoke them directly with an event
# they wrote. The two API functions no longer believe such an event's claims
# (they verify the ID token themselves), but a genuine token replayed that way
# would be served, and authgate's events carry no token at all. So any
# invocation not made by the one service each function exists for pages.
#
# The trail logs these invocations as Lambda data events (cloudtrail.tf).
# Measured on 2026-09-15: a legitimate invocation arrives with
# userIdentity.type "AWSService" -- that is what API Gateway's and Cognito's
# own resource-policy grants look like on the wire -- carrying invokedBy
# "apigateway.amazonaws.com" or "cognito-idp.amazonaws.com". A direct invoke
# by an IAM user arrived as type "IAMUser" with no invokedBy at all. So any
# caller that is not AWSService pages, whatever its invokedBy says or omits,
# including a role that API Gateway, Cognito or some other service assumes to
# call a function on that role's own credentials rather than through the
# function's resource policy. An AWSService caller whose invokedBy names a
# service other than the one each function's resource policy grants pages
# too; an AWSService caller with no invokedBy at all does not -- see "what it
# does not see" below.
#
# Each function is listed as CloudTrail may name it: bare, as an unqualified
# ARN, and by prefix as an ARN qualified with a version or alias. The prefix
# ends in a colon, so scoreboard-api cannot match a future scoreboard-api-v2.
# No event names are listed, so any Lambda data event recorded for these
# functions matches, whatever it is called. Management events never match:
# eventCategory is Data.
#
# What it does not see, the most important first:
#   - A genuine token used through the API. Nothing about that call is
#     unusual; token theft is a session problem. Tokens last an hour
#     (id_token_validity in the scoreboard's admin.tf).
#   - scoreboard-reducer and scoreboard-today. EventBridge invokes the reducer
#     on every game event, so logging it multiplies data-event volume, and a
#     forged invocation corrupts displayed game state without granting control
#     of a panel or a certificate. Scheduler invokes today through its own role,
#     and a direct invoke only republishes today's schedule.
#   - A new integration or route on the scoreboard-admin API, which section 11
#     pages on. A new resource-policy grant (AddPermission) on scoreboard-api
#     or scoreboard-enroll, which section 11 also pages on; the same grant on
#     scoreboard-authgate, which section 10 pages on instead.
#   - Invocations while the trail is not logging, which the audit rule pages on
#     when logging stops or the selectors change.
#   - Rewriting this rule, which section 9 catches.
#   - An AWSService caller with no invokedBy at all. Each of the pattern's
#     first two branches excludes a named invokedBy value with anything-but,
#     which does not match a field that is simply absent -- the opposite of
#     an exists:false branch, which matches only when a field is absent, as
#     section 14 relies on -- and the third branch only catches a caller
#     whose type is not AWSService. So a hypothetical AWSService invocation
#     carrying no invokedBy would match none of the three and would not page.
#     This has not been observed: the resource policies on all three functions
#     grant invoke only to the apigateway.amazonaws.com and
#     cognito-idp.amazonaws.com service principals, and both were seen
#     carrying invokedBy on 2026-09-15. Recorded here rather than fixed,
#     because there is no real record to write the branch against.
#
# A separate API invoking any of the three through its own role, rather than
# through a grant on the function, does page here -- as AssumedRole, caught by
# the third branch's type check, not by either service-specific branch.
locals {
  scoreboard_invoke_arn = { for name, f in data.aws_lambda_function.scoreboard_admin_path : name => f.arn }

  scoreboard_invoke_api_path = [
    "scoreboard-api", local.scoreboard_invoke_arn["scoreboard-api"], { "prefix" = "${local.scoreboard_invoke_arn["scoreboard-api"]}:" },
    "scoreboard-enroll", local.scoreboard_invoke_arn["scoreboard-enroll"], { "prefix" = "${local.scoreboard_invoke_arn["scoreboard-enroll"]}:" },
  ]
  scoreboard_invoke_gate = [
    "scoreboard-authgate", local.scoreboard_invoke_arn["scoreboard-authgate"], { "prefix" = "${local.scoreboard_invoke_arn["scoreboard-authgate"]}:" },
  ]

  # All three functions, for the branch that catches any non-AWSService
  # caller regardless of which function it names.
  scoreboard_invoke_all = concat(local.scoreboard_invoke_api_path, local.scoreboard_invoke_gate)

  scoreboard_invoke_pattern = jsonencode({
    "detail-type" = ["AWS API Call via CloudTrail"]
    "detail" = {
      "eventSource"   = ["lambda.amazonaws.com"]
      "eventCategory" = ["Data"]
      "$or" = [
        { "requestParameters" = { "functionName" = local.scoreboard_invoke_api_path }, "userIdentity" = { "invokedBy" = [{ "anything-but" = ["apigateway.amazonaws.com"] }] } },
        { "requestParameters" = { "functionName" = local.scoreboard_invoke_gate }, "userIdentity" = { "invokedBy" = [{ "anything-but" = ["cognito-idp.amazonaws.com"] }] } },
        { "requestParameters" = { "functionName" = local.scoreboard_invoke_all }, "userIdentity" = { "type" = [{ "anything-but" = ["AWSService"] }] } },
      ]
    }
  })
}

resource "aws_cloudwatch_event_rule" "scoreboard_invoke" {
  name          = "hockeytrack-sec-scoreboard-invoke"
  description   = "Any invocation of scoreboard-api or scoreboard-enroll not made by API Gateway, or of scoreboard-authgate not made by Cognito: someone calling them directly with an event they wrote"
  event_pattern = local.scoreboard_invoke_pattern

  lifecycle {
    precondition {
      condition     = length(local.scoreboard_invoke_pattern) <= 2048
      error_message = "The scoreboard invoke rule's event pattern is ${length(local.scoreboard_invoke_pattern)} characters. EventBridge rejects patterns over 2048, and only at apply."
    }
  }
}

# ---- 13. The scoreboard's supporting control plane ----
#
# Sections 10 to 12 watch who may sign in, what a token is worth, and who may
# call the functions. Each of them leans on four things nothing watched until
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
#   The bulk routes    ExportTableToPointInTime and CreateBackup copy a whole
#                      state table out as a management event, producing no
#                      data event of its own -- section 14 only sees the
#                      tables' row-level traffic, so a bulk export or backup
#                      is invisible to it. PITR is enabled on
#                      scoreboard-enrollments today, so this is not
#                      hypothetical.
#
# Which fields name them. Most rows were confirmed against real events on
# 2026-09-16; the ones marked model-only were not, because no matching event
# occurred in the 90-day window, and come instead from walking the input of
# every write operation in the iam, logs, s3 and cloudfront service models
# shipped with aws-cli 2.33.2 -- the same method section 8 used for the log
# group field list:
#
#   roleName            every IAM write that takes a role; these roles'
#                       policies are all inline, so no write names them only
#                       by policy ARN
#   logGroupName        PutRetentionPolicy, PutMetricFilter,
#                       DeleteMetricFilter, DeleteLogGroup,
#                       PutSubscriptionFilter
#   logGroupIdentifier  model-only: PutTransformer, DeleteTransformer
#                       (rewrite events at ingestion -- blinds the refusal,
#                       crash and mismatch filters), PutDataProtectionPolicy
#                       (masks content, same effect),
#                       PutLogGroupDeletionProtection, PutIndexPolicy,
#                       DeleteIndexPolicy. Takes a name or an ARN, so it is
#                       matched against both forms.
#   resourceArn         model-only for the log groups: TagResource,
#                       PutResourcePolicy, PutDeliverySource (a copy-out
#                       route). Matched against both forms for the same
#                       reason, though a bare name in an ARN-typed field is
#                       not expected to occur. Confirmed for DynamoDB: this
#                       is also where TagResource names a table, since the
#                       service model gives it only ResourceArn -- no table
#                       name form -- so the two table ARNs ride in this same
#                       branch rather than a fifth one.
#   bucketName          S3 bucket writes, which also name the bucket in
#                       resources[]
#   id                  CloudFront UpdateDistribution and DeleteDistribution
#   Resource / resource CloudFront TagResource/UntagResource, a distribution
#                       ARN. Both casings are matched: the service model gives
#                       Resource, but CloudTrail lowercases the first letter
#                       of CloudFront request parameters, the way sections 10
#                       and 11 found for Lambda's identically-modeled
#                       "resource" member. No CloudFront TagResource occurred
#                       in the 90-day window, so this row is model-only too,
#                       and both casings are matched because of that.
#   tableName           confirmed: CreateBackup, UpdateTable, DeleteTable,
#                       UpdateContinuousBackups. Every one of these DynamoDB
#                       management writes names the table by TableName; none
#                       of them takes an ARN, so this branch needs no ARN
#                       form of its own.
#   tableArn            confirmed: ExportTableToPointInTime, the one DynamoDB
#                       management write that names the table only by
#                       TableArn -- it has no TableName parameter at all, so
#                       the tableName branch above cannot see it.
#
# Site deploys stay silent without naming an event, which is how sections 9 to
# 12 are built: CreateInvalidation names the distribution in distributionId,
# which this pattern does not match, while configuration changes name it in id,
# which it does. If CloudFront ever records an invalidation under id, every
# deploy starts paging -- noisy, not blind, and the fix is a field list here.
#
# Measured over ninety days to 2026-09-16: 40,420 management events scanned
# across the four sources, none lacking a readOnly key. The rule as first
# written would have matched 1,368 of them: CreateLogStream 1,333,
# PutRolePolicy 9, CreateRole 7, CreateLogGroup 6, PutRetentionPolicy 6,
# PutMetricFilter 4, PutBucketPolicy 1, PutBucketPublicAccessBlock 1,
# CreateBucket 1. Every Lambda cold start creates a log stream, and that call
# names the group, so it matched. It creates a stream and destroys nothing, so
# it is the one event name this rule excludes -- the same trade the
# invalidation field choice makes, but explicit, because no field
# distinguishes it. What still pages: DeleteLogStream, DeleteLogGroup,
# PutRetentionPolicy, PutMetricFilter, DeleteMetricFilter,
# PutSubscriptionFilter, and everything on the roles and the site. After the
# exclusion the same ninety days hold 35 matches, all scoreboard applies:
# about one every two or three days, and they are deliberate. The exclusion
# sits at the top level, next to $or, and no branch below touches eventName --
# section 8 found that constraining eventName both beside a $or and inside one
# of its branches makes EventBridge's answer depend on JSON key order, but that
# trap needs a branch-level eventName to trigger, and this rule has none, so it
# does not apply here.
#
# The DynamoDB branch was added in a later review round, after the sweep
# above, and was checked differently: not a full scan of every
# dynamodb.amazonaws.com event over ninety days, but a lookup per event name
# this branch's fields cover, against the two tables by name. Over the ninety
# days to 2026-09-16: CreateBackup 0, ExportTableToPointInTime 0, DeleteTable 0
# and TagResource 0 for either table; UpdateTable 1 on scoreboard-devices and
# UpdateContinuousBackups 4 on scoreboard-enrollments, both this repository's
# own applies enabling point-in-time recovery. Five real matches, all
# deliberate -- the same shape as the 35 above, not a new source of noise.
#
# IAM is a global service, but CloudTrail records its calls to us-east-1
# regardless of where the caller sits, so the roleName branch needs no
# regional caveat. The bucket and all six log groups (five function groups
# plus /aws/apigateway/scoreboard-admin) are themselves in us-east-1, and so
# is this rule, so a us-east-1 rule sees their management events without one
# either.
#
# The distribution ID (E3Q7R79Q7PXH26) is a literal, not looked up by name the
# way section 10's pool and section 11's API are. A deleted distribution fails
# this data source outright, at apply, and blocks every other security change
# in this repository's plan until someone edits the ID by hand.
#
# What it does not see:
#   - Objects in the site bucket. PutObject is a data event, and the trail logs
#     object events only for the raw archive, so a page replaced in place is
#     invisible. The distribution's origins and behaviors are what this covers.
#   - A moved domain alias. AssociateAlias names the target distribution, which
#     for an attacker's copy is not this one, and the DNS record lives outside
#     the resources this account watches.
#   - The OAC, a copied distribution or its monitoring subscription.
#     UpdateOriginAccessControl names only the OAC's own Id (the live
#     distribution uses OAC E2JHBLRJXX211W); CopyDistribution names the source
#     in PrimaryDistributionId; Create/DeleteMonitoringSubscription name it in
#     DistributionId. None of those fields is matched here. Swapping a
#     response-headers policy on the distribution's own behaviors is, by
#     contrast, an UpdateDistribution, which does page.
#   - Customer-managed policies. These roles use inline policies only; a
#     managed policy attached later could be widened by a CreatePolicyVersion
#     that names only the policy ARN.
#   - The list-valued log-group fields: logGroupIdentifiers, logGroupArnList
#     and logGroupNames. CreateScheduledQuery, CreateLogAnomalyDetector and
#     PutQueryDefinition carry them, but those are query and anomaly-detector
#     calls, not blinding routes, so they are recorded here rather than added
#     to the pattern.
#   - Identities that can already reach these resources, which section 1 covers
#     for the account's own escalation paths.
#   - Rewriting this rule, which section 9 catches.
#   - A log stream created to impersonate a log source. CreateLogStream is
#     excluded account-wide within this rule's five sources, so one crafted to
#     look like a cold start does not page either.
#   - A restore of either state table. RestoreTableFromBackup names the source
#     only as BackupArn and the copy as TargetTableName, neither of which this
#     rule matches. RestoreTableToPointInTime does name the source table, but
#     as SourceTableName and SourceTableArn -- not TableName or TableArn --
#     so it slips this rule's tableName and tableArn branches the same way.
#     Section 14's gap list carries the same point: the restored copy is a new
#     table, outside both rules, whichever route created it.
#   - A call that names one of these resources only through a field not listed
#     above, the same silent-failure mode sections 8 to 11 share.
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
  scoreboard_role_names                   = sort([for r in data.aws_iam_role.scoreboard : r.name])
  scoreboard_site_bucket                  = data.aws_s3_bucket.scoreboard_site.bucket
  scoreboard_site_distribution            = data.aws_cloudfront_distribution.scoreboard_site.id
  scoreboard_site_alias                   = "scoreboard.davidjdrake.com"
  scoreboard_site_distribution_arn_prefix = "arn:aws:cloudfront::${data.aws_caller_identity.current.account_id}:distribution/${local.scoreboard_site_distribution}"

  # The name form of the two scoreboard log groups, and their ARN form.
  # logGroupName can only hold the name; logGroupIdentifier and resourceArn
  # are matched against both forms below. The ARN form is matched by prefix,
  # not wildcard: EventBridge rejects a field carrying two wildcard values
  # that each hold more than a couple of "*" characters as "too complex" --
  # verified live against this exact pair -- and a fixed region and account
  # need no wildcard character to begin with.
  scoreboard_log_group_names = [{ "wildcard" = "/aws/lambda/scoreboard-*" }, "/aws/apigateway/scoreboard-admin"]
  scoreboard_log_group_arn_prefixes = [
    { "prefix" = "arn:aws:logs:${var.region}:${data.aws_caller_identity.current.account_id}:log-group:/aws/lambda/scoreboard-" },
    { "prefix" = "arn:aws:logs:${var.region}:${data.aws_caller_identity.current.account_id}:log-group:/aws/apigateway/scoreboard-admin" },
  ]

  # The two state tables' name and ARN forms, for the DynamoDB branches
  # below. Reuses data.aws_dynamodb_table.scoreboard_state (cloudtrail.tf),
  # the same lookup that scopes the trail's third selector and section 14,
  # so a renamed table fails this plan rather than silently dropping out of
  # this rule too.
  scoreboard_state_table_names = sort([for t in data.aws_dynamodb_table.scoreboard_state : t.name])
  scoreboard_state_table_arns  = sort([for t in data.aws_dynamodb_table.scoreboard_state : t.arn])

  scoreboard_support_pattern = jsonencode({
    "detail-type" = ["AWS API Call via CloudTrail"]
    "detail" = {
      "eventSource"   = ["iam.amazonaws.com", "logs.amazonaws.com", "s3.amazonaws.com", "cloudfront.amazonaws.com", "dynamodb.amazonaws.com"]
      "eventCategory" = ["Management"]
      "readOnly"      = [false]
      "eventName"     = [{ "anything-but" = ["CreateLogStream"] }]
      "$or" = [
        { "requestParameters" = { "roleName" = local.scoreboard_role_names } },
        { "requestParameters" = { "logGroupName" = local.scoreboard_log_group_names } },
        { "requestParameters" = { "logGroupIdentifier" = concat(local.scoreboard_log_group_names, local.scoreboard_log_group_arn_prefixes) } },
        { "requestParameters" = { "resourceArn" = concat(local.scoreboard_log_group_names, local.scoreboard_log_group_arn_prefixes, local.scoreboard_state_table_arns) } },
        { "requestParameters" = { "bucketName" = [local.scoreboard_site_bucket] } },
        { "requestParameters" = { "id" = [local.scoreboard_site_distribution] } },
        { "requestParameters" = { "Resource" = [{ "prefix" = local.scoreboard_site_distribution_arn_prefix }] } },
        { "requestParameters" = { "resource" = [{ "prefix" = local.scoreboard_site_distribution_arn_prefix }] } },
        { "resources" = { "ARN" = ["arn:aws:s3:::${local.scoreboard_site_bucket}"] } },
        { "requestParameters" = { "tableName" = local.scoreboard_state_table_names } },
        { "requestParameters" = { "tableArn" = local.scoreboard_state_table_arns } },
      ]
    }
  })
}

resource "aws_cloudwatch_event_rule" "scoreboard_support" {
  name          = "hockeytrack-sec-scoreboard-support"
  description   = "Any write naming a scoreboard role, a scoreboard log group, the static site's bucket or distribution, or a management write on the scoreboard's state tables: the routes to widening a role, silencing an alarm, serving a look-alike page, or bulk-copying the tables out from under section 14"
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
# enrollments but only writes devices -- it never reads that table. Their
# calls arrive as userIdentity.type AssumedRole with
# sessionContext.sessionIssuer.arn equal to the role's ARN. ARN, not the
# userName section 12 matches on for invokedBy: a role named scoreboard-api
# in some other account is a different principal with no reason to be
# exempt, and only the ARN says which account issued the session. The ARNs
# are read from data.aws_iam_role.scoreboard (section 13) rather than typed
# as literals, so a role recreated with a new ID keeps resolving and a
# renamed role fails this plan instead of silently exempting nothing. So the
# rule allows those two and pages on everything else, including an IAM user,
# whose events carry no sessionContext at all and need their own branch,
# exactly as section 12's missing invokedBy does.
#
# That branch checks exists:false on sessionIssuer.arn, not on sessionContext
# itself, and the difference is load-bearing. Verified live with aws events
# test-event-pattern: exists:false on sessionContext -- an object-valued
# field once present -- matched a scoreboard-api-issued event as readily as
# an IAM user's, because EventBridge's exists test does not reliably see
# presence or absence of a field whose value is itself an object, only of a
# leaf. sessionIssuer.arn is a leaf: present on every AssumedRole call,
# absent whenever sessionContext is absent altogether, so exists:false on it
# correctly isolates the IAM-user case alone. The same trap would have hit an
# exists:false on invokedBy in section 12 had that field ever been an object
# instead of a string.
#
# The trail logs these as data events, reads included (cloudtrail.tf). No
# event names and no table names are listed in this rule at all: the
# event_selector above is what decides which tables the trail logs, this
# trail carries exactly one AWS::DynamoDB::Table selector, and it names
# exactly these two tables -- so every DynamoDB data event EventBridge can
# deliver already belongs to one of them, whatever operation produced it and
# whatever field that operation names the table in. That is deliberate:
# GetItem, Query, Scan, PutItem, UpdateItem and DeleteItem all name the table
# directly, but BatchGetItem and BatchWriteItem name it inside
# requestItems, TransactWriteItems inside transactItems[].put.tableName,
# ExecuteStatement (PartiQL) inside a statement string, and any of them can
# name it by ARN instead of by name. A requestParameters.tableName clause --
# the first draft of this rule carried one -- matches none of those five
# shapes and was verified live to stay silent on all of them.
# eventCategory is Data, so management writes to the tables stay with
# whatever already covers them.
#
# The cost of dropping the table-name clauses is that this rule is no longer
# scoped to these two tables by its own content, only by what the selector
# above logs: if that selector is ever widened to a third table, this rule
# starts paging on that table's ordinary traffic until the rule is updated to
# match it. That is the loud direction to be wrong in, not the silent one.
#
# A stream is the same shape of loud, not silent. AWS documents that an
# AWS::DynamoDB::Table data-event selector logs the table's stream as well as
# its items, so turning a stream on for either table would make its
# consumer's GetRecords and GetShardIterator calls data events under this
# same selector. The consumer's role is neither scoreboard-api's nor
# scoreboard-enroll's, so it would match the first branch below and page on
# every read of the stream -- continuously, until the rule is updated to
# exempt it or the stream is turned back off. Neither table has a stream
# today.
#
# Expected noise: none. Nothing but the two functions touches these tables
# today, and the 30 days to 2026-09-16 recorded 3 read capacity units across
# both and no writes. The owner's own scan during a recovery does page, which
# is correct: the alert names the caller, and the sentence says to check the
# owners.
#
# What it does not see:
#   - Anyone holding the scoreboard-api or scoreboard-enroll role's
#     credentials, not just the function itself. The match is on
#     sessionIssuer.arn alone, so a credential lifted from either Lambda's
#     environment and replayed from outside AWS -- or from a different
#     function altogether -- still carries that ARN and reads and writes
#     these tables exactly as the real function would. Section 13 pages when
#     the role's policy is widened, section 12 when the function is invoked
#     directly; neither is a substitute for detecting a stolen credential
#     used as itself.
#   - A restore. RestoreTableFromBackup and RestoreTableToPointInTime are
#     management events that name a new table, not either of these two, so
#     the restored copy sits outside this rule and outside section 13's
#     reach until something is pointed at it by name. The backup or export
#     that fed it is not a blind spot any more: section 13 now pages on
#     ExportTableToPointInTime, CreateBackup and the rest of the DynamoDB
#     management writes on these two tables.
#   - Rewriting this rule, which section 9 catches.
locals {
  scoreboard_state_role_arns = [
    data.aws_iam_role.scoreboard["scoreboard-api"].arn,
    data.aws_iam_role.scoreboard["scoreboard-enroll"].arn,
  ]

  scoreboard_state_pattern = jsonencode({
    "detail-type" = ["AWS API Call via CloudTrail"]
    "detail" = {
      "eventSource"   = ["dynamodb.amazonaws.com"]
      "eventCategory" = ["Data"]
      "$or" = [
        {
          "userIdentity" = { "sessionContext" = { "sessionIssuer" = { "arn" = [{ "anything-but" = local.scoreboard_state_role_arns }] } } }
        },
        {
          "userIdentity" = { "sessionContext" = { "sessionIssuer" = { "arn" = [{ "exists" = false }] } } }
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

# ---- 15. The scoreboard's device-image supply chain ----
#
# Sections 10 to 14 watch who may sign in, what a token is worth, who may call
# the functions, who may widen their roles, and who reads the rows. This
# watches what a panel runs. The scoreboard's release workflow builds a
# Raspberry Pi image, publishes it to a mirror, and every panel flashed or
# updated afterwards executes it. That makes the mirror the most valuable
# write target in this account after the archive -- and unlike the archive,
# what it costs is not data but code execution on hardware in someone's home.
#
# Four things carry the chain, and a change to any one of them substitutes an
# image without touching the others:
#
#   The objects       images/<version>/scoreboard-<version>.img.xz is what a
#                     panel is flashed from, and latest.json is what an
#                     unattended panel follows to decide there is a newer one.
#                     Overwriting either serves attacker code on the next
#                     flash or the next update. The bucket is versioned, so
#                     the original survives -- but only if someone notices.
#   The distribution  images.scoreboard.davidjdrake.com is the only name the
#                     panels know. Repointing its origin, or moving the alias
#                     to a different distribution, serves a different bucket
#                     under the same URL with the objects untouched.
#   The publisher     scoreboard-image-publisher is assumed only by GitHub
#                     Actions in the image-release environment, through the
#                     shared GitHub OIDC provider. Widening its trust policy
#                     -- another repository, another environment, a wildcard
#                     subject -- hands the mirror to a workflow nobody
#                     reviewed, and widening its permissions policy hands it
#                     the rest of the account.
#   The OIDC provider The trust policy is only as good as the issuer behind
#                     it. A new client ID or a rewritten thumbprint list
#                     changes which assertions this account accepts, for the
#                     publisher role and for every other role that trusts
#                     GitHub.
#
# The publisher role's own object writes are exempt, by design: that is the
# release workflow doing its job, several times per release, and a rule that
# pages on every release is a rule nobody reads. What guards a malicious
# release through the real workflow is the environment approval on
# image-release and the build attestation the workflow publishes, not this
# rule. The residual is written down honestly in the scoreboard's spec §9.12
# and in docs/threat-model.md §4: a stolen publisher session can overwrite an
# older images/<v>/ object -- one nothing is about to re-publish, so nothing
# disagrees about it -- and this rule will not page. The daily
# scoreboard-imagecheck monitor is what finds the mirror disagreeing with the
# GitHub release, up to 24 hours later.
#
# Which fields name these things. Every row was confirmed against real records
# in the ninety days to 2026-09-17 unless marked model-only:
#
#   resources[].ARN     the object ARN on an S3 data event, matched by prefix
#                       on the bucket ARN plus "/" -- the same shape the
#                       trail's fourth selector uses, so the rule is scoped by
#                       its own content and not only by the selector.
#   bucketName          confirmed: CreateBucket, PutBucketPolicy,
#                       PutBucketVersioning, PutBucketPublicAccessBlock and
#                       PutBucketEncryption on this bucket, the five writes
#                       its creation made on 2026-09-17.
#   resources[].ARN     the bucket ARN on an S3 *management* event, for the
#                       writes that name the bucket only there.
#   id                  confirmed: UpdateDistribution, 17 records, all
#                       requestParameters {distributionConfig, id, ifMatch}.
#                       DeleteDistribution is model-only here but takes the
#                       same {Id, IfMatch} pair, and the sibling deletes that
#                       did occur -- DeleteOriginAccessControl, DeletePublicKey
#                       -- both recorded {id, ifMatch}.
#   targetDistributionId  model-only: AssociateAlias, whose only reference to
#                       the distribution is TargetDistributionId. Moving the
#                       alias to an attacker's distribution names that one, not
#                       this one, and is a gap section 13 records too; moving it
#                       *back*, or onto this one, is what this branch sees.
#   Resource/resource   confirmed lowercase: TagResource and UntagResource, one
#                       record each, both requestParameters {resource, ...}
#                       carrying a distribution ARN. Both casings are matched
#                       anyway, as section 13 does, because the service model
#                       says Resource and CloudTrail lowercases the first
#                       letter of CloudFront request parameters.
#   roleName            confirmed: PutRolePolicy on scoreboard-image-publisher,
#                       and CreateRole, on 2026-09-17. Matched exactly, not by
#                       prefix, which is what keeps scoreboard-imagecheck -- the
#                       monitor's own role, whose policy this repository and the
#                       scoreboard both touch -- out of the rule.
#   openIDConnectProviderArn  model-only: every IAM write on a provider takes
#                       OpenIDConnectProviderArn and nothing else that names it.
#                       No such call occurred in the window; the provider
#                       predates it.
#
# Why CreateInvalidation is deliberately absent. It is the one frequent write
# that names a distribution, and the measurement says how frequent: 487
# invalidations in ninety days across eight distributions in this account, 250
# of them on one. None named this distribution, which has existed for a day.
# It names its target in distributionId -- 487 records, no exceptions -- and
# this rule matches id, targetDistributionId and the ARN forms, never
# distributionId, so no eventName exclusion is needed to keep invalidations
# quiet: no branch can see them. That is the same field choice section 13
# makes, and the scoreboard's spec §9.8 states the reason: an invalidation only
# makes CloudFront re-read an origin whose every write already pages here. If
# CloudFront ever records an invalidation under id instead, every deploy starts
# paging -- noisy, not blind, and the fix is a field list here.
#
# Expected noise: none, and it is measured rather than assumed. Counting only
# the four things this rule watches, by a lookup-events ResourceName query per
# resource over the ninety days to 2026-09-17: five management writes on the
# bucket (CreateBucket, PutBucketEncryption, PutBucketPublicAccessBlock,
# PutBucketVersioning, PutBucketPolicy), two on the publisher role (CreateRole,
# PutRolePolicy), none on the distribution and none on the OIDC provider.
# Seven in total, every one of them the single Terraform apply on 2026-09-17
# that created them. The same apply made other writes -- scoreboard-imagecheck's
# role and function, the distribution's own creation, its origin access control
# -- but none of those is one of the four things counted here, and the ones
# that are not are not matched by this rule either. There were no object writes
# at all, because no release has been published yet. The
# steady state is a handful of matches per release, all of them the publisher
# role's and all of them exempt.
#
# The two data branches carry readOnly false as well as the selector's
# write-only scope. That readOnly is really present on an S3 data event was
# confirmed against real records rather than assumed -- lookup-events returns
# management events only, so this was checked in the trail's log group, where
# the archive's own PutObject and DeleteObject records carry
# eventCategory "Data", readOnly false, and a resources[] holding the bucket
# ARN and the object ARN. A field this rule gated on that turned out to be
# absent would make both branches silently never match, which is the failure
# this file exists to avoid. It is deliberate belt and braces: the selector
# decides what CloudTrail logs, the branch decides what pages, and if the
# selector is ever widened to "All" -- the way cloudtrail_archive_read_events
# widens the archive's -- this rule keeps its shape instead of paging on every
# CloudFront origin fetch.
#
# That log-group evidence proves CloudTrail *logs* S3 object data events in
# this shape; it does not prove EventBridge *delivers* them. Sections 12 and 14
# rest on delivery that has been seen in this account -- Lambda and DynamoDB
# data events both -- but no S3 object data event has yet been delivered to a
# rule here, because the selector above is new and the mirror has never been
# written to. That is the single unobserved assumption in this rule, and the
# Task 9 break test closes it: a write and a delete in the mirror by a
# principal that is not the publisher role should produce two alerts.
#
# The second data branch tests exists:false on sessionIssuer.arn, the leaf,
# not on sessionContext, the object above it. Section 14 found live that
# EventBridge's exists test does not reliably see an object-valued field's
# presence, so exists:false on sessionContext matched a role's own event as
# readily as an IAM user's. The leaf is present on every AssumedRole call and
# absent whenever sessionContext is, so this branch isolates the IAM-user and
# root cases -- and makes an identity shape nobody anticipated page rather
# than go quiet.
#
# Every branch repeats its own eventSource, eventCategory and readOnly
# constraints, and nothing is constrained beside the $or. Section 8 found that
# constraining one field both beside a $or and inside a branch makes
# EventBridge's verdict depend on JSON key order, and jsonencode always sorts
# "$or" first, which is the broken order.
#
# The bucket-by-ARN branch carries eventCategory Management for a reason worth
# stating: an S3 data event's resources[] lists the bucket ARN alongside the
# object ARN, so without that constraint every release's own object writes
# would match this branch and page -- the exact case the publisher exemption
# exists to prevent.
#
# What it does not see, the most important first:
#   - The publisher role's own writes to the mirror. See above; this is the
#     designed exemption and the biggest residual in the rule.
#   - A tampered GitHub release asset. A GitHub organization admin can replace
#     what the workflow uploaded, and no AWS API call occurs. The monitor
#     compares the mirror against the release, so this shows up as the two
#     disagreeing only if the mirror still holds the original.
#   - An invalidation, by anyone. See above.
#   - A moved alias, in the direction that matters: AssociateAlias onto an
#     attacker's copy names that distribution, not this one, and the DNS record
#     lives outside this account's resources. Section 13 records the same gap
#     for the site.
#   - A second distribution stood up in front of the same origin.
#     CreateDistributionWithTags carries only distributionConfigWithTags, and
#     CreateDistribution only distributionConfig: neither names an existing
#     distribution's id, and the new distribution's own id and ARN appear only
#     in responseElements, which this rule does not match. Confirmed against
#     all five CreateDistributionWithTags records in the ninety days to
#     2026-09-17 -- one of which created this very distribution. The origin
#     bucket's policy names the distribution its OAC belongs to, so serving
#     the real objects through a copy needs a PutBucketPolicy, which the
#     bucket branches above do page on; and serving them under the real
#     hostname needs the DNS move listed above, which is outside this account.
#     So this is a completeness gap rather than a standalone route, and it is
#     recorded rather than closed: matching every CreateDistribution* in the
#     account would page on every unrelated distribution this account creates.
#   - CopyDistribution, Create/DeleteMonitoringSubscription and
#     UpdateOriginAccessControl, which name the distribution in
#     primaryDistributionId, distributionId and the OAC's own id respectively
#     -- none of them fields this rule matches. distributionId is excluded on
#     purpose, and the price of that choice is these two.
#   - Anyone holding the publisher role's credentials rather than the role
#     itself, which is what the exemption means in practice, and the same gap
#     section 14 carries for the two function roles.
#   - Objects in the bucket while the trail is not logging, which the audit
#     rule pages on when logging stops or the selectors change.
#   - Rewriting this rule, which section 9 catches.
data "aws_iam_role" "scoreboard_image_publisher" {
  name = "scoreboard-image-publisher"
}

# The shared GitHub OIDC provider, looked up by URL rather than written as an
# ARN literal: this account has exactly one, several roles trust it, and a
# deleted provider fails this plan instead of leaving a branch matching an ARN
# that no longer exists.
data "aws_iam_openid_connect_provider" "github" {
  url = "https://token.actions.githubusercontent.com"
}

data "aws_cloudfront_distribution" "scoreboard_images" {
  id = var.scoreboard_images_distribution_id
}

locals {
  scoreboard_images_bucket_arn    = data.aws_s3_bucket.scoreboard_images.arn
  scoreboard_images_publisher_arn = data.aws_iam_role.scoreboard_image_publisher.arn
  scoreboard_images_alias         = "images.scoreboard.davidjdrake.com"
  scoreboard_images_dist_arn      = "arn:aws:cloudfront::${data.aws_caller_identity.current.account_id}:distribution/${var.scoreboard_images_distribution_id}"

  scoreboard_image_pattern = jsonencode({
    "detail-type" = ["AWS API Call via CloudTrail"]
    "detail" = {
      "$or" = [
        # An object write in the mirror whose session was issued by some role
        # other than the publisher's.
        {
          "eventSource"   = ["s3.amazonaws.com"]
          "eventCategory" = ["Data"]
          "readOnly"      = [false]
          "resources"     = { "ARN" = [{ "prefix" = "${local.scoreboard_images_bucket_arn}/" }] }
          "userIdentity"  = { "sessionContext" = { "sessionIssuer" = { "arn" = [{ "anything-but" = [local.scoreboard_images_publisher_arn] }] } } }
        },
        # The same write with no session issuer at all: an IAM user, root, or
        # an identity shape not seen before.
        {
          "eventSource"   = ["s3.amazonaws.com"]
          "eventCategory" = ["Data"]
          "readOnly"      = [false]
          "resources"     = { "ARN" = [{ "prefix" = "${local.scoreboard_images_bucket_arn}/" }] }
          "userIdentity"  = { "sessionContext" = { "sessionIssuer" = { "arn" = [{ "exists" = false }] } } }
        },
        # A management write naming the bucket by name.
        {
          "eventSource"       = ["s3.amazonaws.com"]
          "eventCategory"     = ["Management"]
          "readOnly"          = [false]
          "requestParameters" = { "bucketName" = [data.aws_s3_bucket.scoreboard_images.bucket] }
        },
        # A management write naming the bucket only in resources[]. Management,
        # or every object write above would match it too.
        {
          "eventSource"   = ["s3.amazonaws.com"]
          "eventCategory" = ["Management"]
          "readOnly"      = [false]
          "resources"     = { "ARN" = [local.scoreboard_images_bucket_arn] }
        },
        # The distribution, by ID, by alias target, and by ARN in both casings.
        { "eventSource" = ["cloudfront.amazonaws.com"], "readOnly" = [false], "requestParameters" = { "id" = [var.scoreboard_images_distribution_id] } },
        { "eventSource" = ["cloudfront.amazonaws.com"], "readOnly" = [false], "requestParameters" = { "targetDistributionId" = [var.scoreboard_images_distribution_id] } },
        { "eventSource" = ["cloudfront.amazonaws.com"], "readOnly" = [false], "requestParameters" = { "Resource" = [{ "prefix" = local.scoreboard_images_dist_arn }] } },
        { "eventSource" = ["cloudfront.amazonaws.com"], "readOnly" = [false], "requestParameters" = { "resource" = [{ "prefix" = local.scoreboard_images_dist_arn }] } },
        # The publisher role, by exact name, and the issuer its trust rests on.
        { "eventSource" = ["iam.amazonaws.com"], "readOnly" = [false], "requestParameters" = { "roleName" = [data.aws_iam_role.scoreboard_image_publisher.name] } },
        { "eventSource" = ["iam.amazonaws.com"], "readOnly" = [false], "requestParameters" = { "openIDConnectProviderArn" = [data.aws_iam_openid_connect_provider.github.arn] } },
      ]
    }
  })
}

resource "aws_cloudwatch_event_rule" "scoreboard_image" {
  name          = "hockeytrack-sec-scoreboard-image"
  description   = "Any write to the scoreboard device-image mirror not made by the publisher role, or any change to its bucket, distribution, publisher role or GitHub OIDC provider: the routes to running attacker code on every panel flashed afterwards"
  event_pattern = local.scoreboard_image_pattern

  lifecycle {
    precondition {
      condition     = can(regex("^E[A-Z0-9]+$", var.scoreboard_images_distribution_id))
      error_message = "scoreboard_images_distribution_id is \"${var.scoreboard_images_distribution_id}\", which is not a CloudFront distribution ID (^E[A-Z0-9]+$). Set it in terraform.tfvars."
    }
    precondition {
      condition     = contains(data.aws_cloudfront_distribution.scoreboard_images.aliases, local.scoreboard_images_alias)
      error_message = "Distribution ${var.scoreboard_images_distribution_id} does not serve ${local.scoreboard_images_alias}. A well-formed ID that names the wrong distribution would leave the image mirror unwatched."
    }
    precondition {
      condition     = length(local.scoreboard_image_pattern) <= 2048
      error_message = "The scoreboard image rule's event pattern is ${length(local.scoreboard_image_pattern)} characters. EventBridge rejects patterns over 2048, and only at apply."
    }
  }
}
