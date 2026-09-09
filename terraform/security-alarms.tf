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
#                                              must first enrol one, which is
#                                              the identity rule.
#   Remove the bucket policy, then delete   -> also needs MFA, so the identity
#                                              rule again, and
#                                              DeleteBucketPolicy itself is the
#                                              archive rule.
#   Take over root, then act as root        -> account-contact rule, then the
#                                              root sign-in alarm.
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
    identity = aws_cloudwatch_event_rule.identity_escalation
    audit    = aws_cloudwatch_event_rule.audit_tampering
    archive  = aws_cloudwatch_event_rule.archive_tampering
    alerting = aws_cloudwatch_event_rule.alerting_tampering
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

  security_alert_template = "\"HOCKEYTRACK SECURITY: <event> in account <account> (<region>) at <time>.\\nActor: <actor>\\nSource IP: <ip>\\n\\nIf this was not you, assume the archive's MFA gate is bypassed. Recovery procedure: docs/threat-model.md, section 7.\""
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
# The irreducible residual: deleting THIS rule is itself unalarmed. Closing that
# needs a second account, which is the HOC-55 conversation.
resource "aws_cloudwatch_event_rule" "alerting_tampering" {
  name        = "hockeytrack-sec-alerting-tampering"
  description = "The security alarming itself being removed or disabled"
  event_pattern = jsonencode({
    "source"      = ["aws.events", "aws.sns", "aws.cloudwatch"]
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
    input_template = local.security_alert_template
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
