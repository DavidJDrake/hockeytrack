resource "aws_s3_bucket" "raw" {
  bucket = "hockeytrack-raw-${data.aws_caller_identity.current.account_id}"

  # Catches an accidental `terraform destroy` at plan time rather than at the
  # API, which matters more here than anywhere else in the stack.
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_public_access_block" "raw" {
  bucket                  = aws_s3_bucket.raw.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# The raw archive is write-once; versioning makes an accidental delete or
# overwrite reversible. Old versions age out after 90 days.
resource "aws_s3_bucket_versioning" "raw" {
  bucket = aws_s3_bucket.raw.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "raw" {
  bucket     = aws_s3_bucket.raw.id
  depends_on = [aws_s3_bucket_versioning.raw]
  rule {
    id     = "expire-noncurrent"
    status = "Enabled"
    filter {}
    # newer_noncurrent_versions is the load-bearing half. Overwriting an object
    # is an allowed s3:PutObject, and without this the previous version becomes
    # noncurrent and S3 deletes it 90 days later on the writer's behalf — no
    # denied API call anywhere in that sequence. Lifecycle expiry is performed
    # by S3 itself, not by a principal, so no bucket policy can intervene.
    # Keeping the five most recent noncurrent versions unconditionally means a
    # single overwrite, whether hostile or a bug in the ingest path, is always
    # recoverable rather than merely recoverable for a quarter.
    noncurrent_version_expiration {
      noncurrent_days           = 90
      newer_noncurrent_versions = 5
    }
  }
}

# ---- Tamper resistance (HOC-54) ----
#
# Read the limits of this control before trusting it.
#
# What it is. Every action that would destroy or expose the archive is denied
# unless the caller authenticated with MFA. The condition is BoolIfExists,
# because aws:MultiFactorAuthPresent is *absent* rather than false on a call
# made with long-term access keys; a plain Bool test would have exempted
# exactly the stolen-key case it exists to stop. s3:PutLifecycleConfiguration
# is on the list because lifecycle rules make S3 delete data on the caller's
# behalf, with no delete API call to deny. s3:PutBucketPolicy and
# s3:DeleteBucketPolicy are on the list because a control a stolen key can
# remove first is not a control. The ownership, ACL and encryption actions are
# on the list because each is an indirect route to the same outcome: re-enable
# ACLs and grant a foreign account WRITE, or rewrite every object under an
# attacker-held KMS key and schedule that key for deletion.
#
# What it is NOT. This does not stop this account's administrator credential,
# and it is important to be exact about why. AdministratorAccess includes
# iam:CreateVirtualMFADevice and iam:EnableMFADevice, so whoever holds that key
# can enrol an MFA device of their own choosing, call sts:GetSessionToken with
# it, and satisfy this condition legitimately. Against that principal the
# statement is worth about five API calls. It is a real control against a
# careless operator, an accidental destroy, and any credential scoped away from
# IAM; it is a speed bump against a compromised admin key, and the CloudTrail
# record it forces is arguably worth more than the delay.
#
# The controls that would actually hold are Object Lock in COMPLIANCE mode,
# which not even the root user can override, and a copy in a separate AWS
# account. Both are tracked separately. Neither is implemented here, and this
# comment exists so nobody reads the policy below and concludes otherwise.
#
# No AWS service principal is exempted. Nothing in this system performs these
# actions on our behalf today, so the exemption would be unearned reach.
# Adding S3 Inventory, Storage Lens, replication or Batch Operations against
# this bucket later will require an explicit carve-out here; that shows up as
# an AccessDenied at setup time, which is the failure mode we want.
#
# This cannot lock the account out. An MFA-authenticated session edits the
# policy normally, and AWS guarantees the bucket owner's root principal can
# always call Get/Put/DeleteBucketPolicy "even if their bucket policy
# explicitly denies the root principal's access" (S3 API reference,
# DeleteBucketPolicy). Root on this account has an MFA device; the day-to-day
# IAM user does not, which is the ordering that makes this safe to apply now.
data "aws_iam_policy_document" "raw_tamper" {
  statement {
    sid    = "DenyDestructiveActionsWithoutMFA"
    effect = "Deny"
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    actions = [
      "s3:BypassGovernanceRetention",
      "s3:DeleteBucket",
      "s3:DeleteBucketPolicy",
      "s3:DeleteObject",
      "s3:DeleteObjectVersion",
      "s3:PutBucketAcl",
      "s3:PutBucketObjectLockConfiguration",
      "s3:PutBucketOwnershipControls",
      "s3:PutBucketPolicy",
      "s3:PutBucketPublicAccessBlock",
      "s3:PutBucketRequestPayment",
      "s3:PutBucketVersioning",
      "s3:PutEncryptionConfiguration",
      "s3:PutLifecycleConfiguration",
      "s3:PutObjectAcl",
      "s3:PutObjectLegalHold",
      "s3:PutObjectRetention",
      "s3:PutReplicationConfiguration",
    ]
    resources = [aws_s3_bucket.raw.arn, "${aws_s3_bucket.raw.arn}/*"]
    condition {
      test     = "BoolIfExists"
      variable = "aws:MultiFactorAuthPresent"
      values   = ["false"]
    }
  }

  # s3:PutObject has to stay allowed — it is the only write the ingest path
  # makes — which leaves copy-over-self under an attacker-supplied KMS key as a
  # way to render every object undecryptable without deleting anything. Nothing
  # here ever writes a KMS-encrypted object, so refusing them outright costs the
  # system nothing and closes that path. Unconditional rather than MFA-gated:
  # there is no legitimate caller, so there is no case to leave open.
  statement {
    sid    = "DenyKMSEncryptedWrites"
    effect = "Deny"
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.raw.arn}/*"]
    condition {
      test     = "StringEquals"
      variable = "s3:x-amz-server-side-encryption"
      values   = ["aws:kms", "aws:kms:dsse"]
    }
  }
}

resource "aws_s3_bucket_policy" "raw" {
  bucket = aws_s3_bucket.raw.id
  policy = data.aws_iam_policy_document.raw_tamper.json
}
