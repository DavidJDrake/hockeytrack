# Require a second factor for escalation and for silencing detection (HOC-53).
#
# Ships DISABLED. Enabling it before an MFA device exists on the user would
# lock that user out of the very calls needed to enrol one, leaving root as the
# only way back in. Enrol first, prove `aws sts get-session-token` works, then
# set enforce_mfa_on_admin_user = true. tools/mfa-session.sh does the enrolling
# half's day-to-day counterpart.
#
# What this is for. HOC-54 gated the archive on aws:MultiFactorAuthPresent, and
# that file is candid that an admin key defeats it in about five calls, because
# AdministratorAccess includes iam:CreateVirtualMFADevice and
# iam:EnableMFADevice -- the holder simply grants themselves the factor. This
# policy closes exactly that loop: enrolling or removing an MFA device is itself
# denied without MFA. A key with no MFA session can no longer manufacture one.
# The deliberate consequence is that a lost MFA device is recovered through
# root, which has its own device. That is the trade, and it is the right way
# round: the alternative leaves the archive's protection self-defeating.
#
# The action list is not "everything destructive". It is the four things an
# attacker holding this key must do that it cannot already do trivially:
# manufacture a second factor, establish persistence, silence the alarming, and
# destroy the evidence. Bulk data operations are already covered by the bucket
# policy in s3.tf, and duplicating them here would buy nothing.
#
# Role mutations ARE included, and that is the expensive part. Without them the
# policy is ornamental: create a role, attach admin, assume it, and the session
# is no longer this user, so a user-attached deny stops applying. Including them
# means an ordinary `make deploy` that touches a role policy needs an MFA
# session. That cost is real and is the reason for the helper script; a control
# that makes the routine path painful is a control that gets removed.
variable "enforce_mfa_on_admin_user" {
  description = "Deny escalation and detection-tampering actions to the admin user without MFA. Enable ONLY after an MFA device is enrolled and a session token is proven to work."
  type        = bool
  default     = false
}

variable "admin_user_name" {
  description = "IAM user the MFA enforcement policy attaches to."
  type        = string
  default     = "funandgames"
}

data "aws_iam_policy_document" "require_mfa" {
  statement {
    sid    = "DenyEscalationAndCoverUpWithoutMFA"
    effect = "Deny"
    actions = [
      # Manufacturing the second factor the archive policy depends on.
      "iam:CreateVirtualMFADevice", "iam:DeleteVirtualMFADevice",
      "iam:EnableMFADevice", "iam:DeactivateMFADevice",
      "iam:ResyncMFADevice",
      # The same thing by way of federation, no device required.
      "iam:CreateSAMLProvider", "iam:UpdateSAMLProvider",
      "iam:DeleteSAMLProvider", "iam:CreateOpenIDConnectProvider",
      # Persistence: new credentials, new identities.
      "iam:CreateAccessKey", "iam:UpdateAccessKey",
      "iam:CreateUser", "iam:CreateLoginProfile", "iam:UpdateLoginProfile",
      "iam:AttachUserPolicy", "iam:PutUserPolicy",
      # Roles, without which the rest is ornamental: a new role with admin
      # attached is a session this user-scoped deny no longer governs.
      "iam:CreateRole", "iam:DeleteRole", "iam:UpdateAssumeRolePolicy",
      "iam:AttachRolePolicy", "iam:DetachRolePolicy",
      "iam:PutRolePolicy", "iam:DeleteRolePolicy",
      # Taking the account itself, via the root email.
      "account:PutContactInformation", "account:PutAlternateContact",
      "account:StartPrimaryEmailUpdate", "account:AcceptPrimaryEmailUpdate",
      # Silencing the recorder.
      "cloudtrail:StopLogging", "cloudtrail:DeleteTrail",
      "cloudtrail:UpdateTrail", "cloudtrail:PutEventSelectors",
      # Silencing the pager (HOC-56).
      "events:DeleteRule", "events:RemoveTargets", "events:DisableRule",
      "sns:DeleteTopic", "sns:RemovePermission",
      "cloudwatch:DeleteAlarms", "cloudwatch:DisableAlarmActions",
      # Destroying the evidence.
      "logs:DeleteLogGroup", "logs:DeleteMetricFilter",
      "kms:ScheduleKeyDeletion", "kms:DisableKey",
    ]
    resources = ["*"]
    condition {
      test     = "BoolIfExists"
      variable = "aws:MultiFactorAuthPresent"
      values   = ["false"]
    }
  }
}

resource "aws_iam_user_policy" "require_mfa" {
  count  = var.enforce_mfa_on_admin_user ? 1 : 0
  name   = "hockeytrack-require-mfa"
  user   = var.admin_user_name
  policy = data.aws_iam_policy_document.require_mfa.json
}
