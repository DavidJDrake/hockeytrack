# A deny boundary on the two IAM users that belong to other projects (HOC-58).
#
# Why a deny boundary rather than replacing AdministratorAccess with a policy
# scoped to what the credential uses, which is what HOC-58 originally proposed.
# Ninety days of CloudTrail, paged to completion, says healthtracker-deploy
# made 1,250 calls as the user itself across eighteen services -- 440
# logs:FilterLogEvents, 354 sts:AssumeRole into the cdk-hnb659fds-* roles, plus
# kms, cloudfront, secretsmanager, lambda, cognito, cloudformation, route53 and
# apigateway. A policy scoped to that would have to allow eighteen services,
# which removes almost nothing, while any service it happens not to have
# touched in ninety days becomes a broken deploy later. The instrument does not
# fit the problem.
#
# What does fit: deny these identities access to THIS project's resources,
# which the same evidence says they have never used -- one ListBuckets from
# healthtracker-deploy on 2026-08-01, and nothing at all from davidjdrake, in
# three months. So this cannot break their work, because it forbids only what
# they demonstrably do not do, while removing this project from their blast
# radius. An explicit deny beats any allow, including AdministratorAccess, so
# it holds even though both users keep admin.
#
# Every resource list below is scoped to this project on purpose. The account
# is shared: it also carries EbookShare-* and HealthTracker-prod-* alarms. A
# deny on "*" for DeleteAlarms would read as tidier and would break those
# projects' stack teardowns, which is exactly the availability-for-security
# trade HOC-58 warns against making on someone else's behalf.
#
# Managed rather than inline, which the first attempt got wrong: davidjdrake
# already spends 1,638 of its 2,048-byte inline budget on two claude-* policies,
# so an inline attachment failed with LimitExceeded. A managed policy has its
# own 6,144-byte limit and does not draw on that budget.
#
# This is not a substitute for an account boundary. A credential with admin can
# detach this policy. HOC-55 is where the control that survives an admin
# credential gets decided; this narrows the everyday blast radius.
data "aws_iam_policy_document" "foreign_project_deny" {
  # The archive itself.
  statement {
    sid       = "DenyThisProjectsArchive"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.raw.arn, "${aws_s3_bucket.raw.arn}/*"]
  }

  # Going quiet before doing anything else. Nothing in the sweep touched
  # CloudTrail at all, so denying the trail costs these identities nothing.
  statement {
    sid    = "DenyAuditTrailTampering"
    effect = "Deny"
    actions = [
      "cloudtrail:DeleteTrail",
      "cloudtrail:PutEventSelectors",
      "cloudtrail:StopLogging",
      "cloudtrail:UpdateTrail",
    ]
    resources = [aws_cloudtrail.account.arn]
  }

  # This project's alarms only -- see the note above about the other projects'
  # alarms living in the same account.
  statement {
    sid    = "DenySilencingThisProjectsAlarms"
    effect = "Deny"
    actions = [
      "cloudwatch:DeleteAlarms",
      "cloudwatch:DisableAlarmActions",
      "cloudwatch:SetAlarmState",
    ]
    resources = [
      "arn:aws:cloudwatch:*:${data.aws_caller_identity.current.account_id}:alarm:hockeytrack-*",
      "arn:aws:cloudwatch:*:${data.aws_caller_identity.current.account_id}:alarm:scoreboard-*",
    ]
  }
}

resource "aws_iam_policy" "foreign_project_deny" {
  name        = "hockeytrack-foreign-project-deny"
  description = "Denies identities belonging to other projects any access to HockeyTrack's archive, audit trail and alarms (HOC-58)."
  policy      = data.aws_iam_policy_document.foreign_project_deny.json
}

resource "aws_iam_user_policy_attachment" "foreign_project_deny" {
  for_each = toset(["healthtracker-deploy", "davidjdrake"])

  user       = each.key
  policy_arn = aws_iam_policy.foreign_project_deny.arn
}
