# Account-level audit log. Covers both this project and the scoreboard, which
# share this account -- the account is the trust boundary, not the repository,
# so the trail lives here in the platform stack rather than being duplicated.
#
# Deliberately its own bucket, not the raw archive: an audit log stored inside
# the blast radius it exists to describe is not an audit log. Whatever can
# destroy the archive should not also be able to erase the record of doing so.

resource "aws_s3_bucket" "trail" {
  bucket = "hockeytrack-cloudtrail-${data.aws_caller_identity.current.account_id}"

  # Same reasoning as the archive: catch an accidental destroy at plan time
  # rather than at the API. Losing the evidence is worse than losing a Lambda.
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_public_access_block" "trail" {
  bucket                  = aws_s3_bucket.trail.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "trail" {
  bucket = aws_s3_bucket.trail.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# Log file validation proves a delivered log was not altered. It says nothing
# about one that is no longer there, and deleting the log is the standard second
# move after doing something worth logging. Versioning makes that recoverable.
resource "aws_s3_bucket_versioning" "trail" {
  bucket = aws_s3_bucket.trail.id
  versioning_configuration {
    status = "Enabled"
  }
}

# Log volume on a single-user account is small, but unbounded retention is an
# unbounded bill. A year is long enough to investigate something noticed late.
#
# Versioning changes what `expiration` means here: it no longer deletes, it
# writes a delete marker and leaves the object as a noncurrent version. Without
# the second rule below, storage would grow forever. With it, a log that ages
# out is really gone ninety days later, and a log someone DELETES is likewise
# recoverable for ninety days -- the same window the archive has.
#
# Note the real retention is cloudtrail_retention_days + 90, not the variable
# alone: the noncurrent clock starts when the successor version is created,
# which is when the expiration rule plants the delete marker. Trivial in dollars
# at this volume, but the variable name reads like the whole story and is not.
#
# Deliberately no newer_noncurrent_versions here, though it is exactly what
# s3.tf needed. The archive's objects get overwritten, so retaining the five
# most recent noncurrent versions costs almost nothing. Here a key accumulates
# at most one noncurrent version -- log files carry a random disambiguator so
# their keys never collide, and digest keys, which ARE deterministic and can be
# redelivered, would reach two at worst. Either way the count never exceeds
# five, both lifecycle conditions must be exceeded for a deletion to occur, so
# the retain-five rule would match every version forever and quietly defeat the
# retention policy. Same knob, opposite effect, because the write pattern
# differs.
#
# Choosing lifecycle here also forecloses MFA Delete, which CloudTrail's own
# best-practice guidance suggests for a log bucket: AWS does not support
# lifecycle configuration on an MFA-Delete-enabled bucket. Bounded cost is worth
# more than that on a personal account, but it is a trade, not an oversight.
resource "aws_s3_bucket_lifecycle_configuration" "trail" {
  bucket     = aws_s3_bucket.trail.id
  depends_on = [aws_s3_bucket_versioning.trail]
  rule {
    id     = "expire-logs"
    status = "Enabled"
    filter {}
    expiration {
      days = var.cloudtrail_retention_days
    }
    noncurrent_version_expiration {
      noncurrent_days = 90
    }
  }
}

# CloudTrail writes as a service principal, so it needs explicit permission.
# Both statements are scoped by aws:SourceArn to this trail, so another
# account's trail cannot be pointed at this bucket.
data "aws_iam_policy_document" "trail_bucket" {
  # Parity with s3.tf. s3:PutObject stays allowed so CloudTrail can deliver,
  # which leaves copy-over-self under an attacker-supplied KMS key as a way to
  # render the whole trail unreadable without deleting anything -- then delete
  # the key. This trail has no kms_key_id, so CloudTrail writes SSE-S3 and never
  # sends this header; refusing it costs nothing today. It does mean enabling
  # SSE-KMS on the trail later requires editing this policy first, which will
  # itself need MFA. That fails loudly at trail-update time rather than quietly.
  statement {
    sid    = "DenyKMSEncryptedWrites"
    effect = "Deny"
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.trail.arn}/*"]
    condition {
      test     = "StringEquals"
      variable = "s3:x-amz-server-side-encryption"
      values   = ["aws:kms", "aws:kms:dsse"]
    }
  }

  statement {
    sid       = "AWSCloudTrailAclCheck"
    actions   = ["s3:GetBucketAcl"]
    resources = [aws_s3_bucket.trail.arn]
    principals {
      type        = "Service"
      identifiers = ["cloudtrail.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceArn"
      values   = ["arn:aws:cloudtrail:${var.region}:${data.aws_caller_identity.current.account_id}:trail/hockeytrack-account"]
    }
  }
  # The audit log deserves the protection the archive got. An attacker who
  # trips an alarm should not then be able to erase what recorded it.
  #
  # Identical action list to s3.tf, and identical reasoning: BoolIfExists
  # because the key is absent rather than false on a long-lived access key;
  # PutLifecycleConfiguration because lifecycle rules make S3 delete on the
  # caller's behalf with no delete API to deny; the policy's own management
  # actions because a control a stolen key can remove first is not a control.
  #
  # s3:PutObject is deliberately absent, which is what keeps CloudTrail working:
  # the service writes and never deletes, and its calls carry no MFA context, so
  # BoolIfExists matches it and every listed action IS denied to CloudTrail.
  #
  # s3:PutObjectAcl is absent for a subtler reason, and s3.tf's identical list is
  # no guide here. The Allow above is conditioned on s3:x-amz-acl, which can only
  # match if the header is present -- so CloudTrail demonstrably sends
  # `x-amz-acl: bucket-owner-full-control` on every delivery. AWS documents
  # s3:PutObjectAcl as conditionally required for a PutObject carrying an ACL
  # header, and while an Allow on it is not needed for that specific canned ACL,
  # nothing documents a Deny being skipped. Deny beats Allow, and the failure
  # would be delivery silently stopping while IsLogging still reads true.
  # Denying it buys nothing anyway: Object Ownership is BucketOwnerEnforced, so
  # object ACLs are refused outright, and re-enabling them needs
  # s3:PutBucketOwnershipControls, which IS denied below.
  #
  # Same ceiling as everywhere else in this estate: this stops a careless
  # operator and a credential scoped away from IAM, not an admin key, which can
  # grant itself the second factor. terraform/iam-mfa.tf closes that loop and
  # also covers cloudtrail:StopLogging and DeleteTrail at the identity layer,
  # so the trail resource itself needs nothing further here.
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
      "s3:PutObjectLegalHold",
      "s3:PutObjectRetention",
      "s3:PutReplicationConfiguration",
    ]
    resources = [aws_s3_bucket.trail.arn, "${aws_s3_bucket.trail.arn}/*"]
    condition {
      test     = "BoolIfExists"
      variable = "aws:MultiFactorAuthPresent"
      values   = ["false"]
    }
  }

  statement {
    sid       = "AWSCloudTrailWrite"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.trail.arn}/AWSLogs/${data.aws_caller_identity.current.account_id}/*"]
    principals {
      type        = "Service"
      identifiers = ["cloudtrail.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "s3:x-amz-acl"
      values   = ["bucket-owner-full-control"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceArn"
      values   = ["arn:aws:cloudtrail:${var.region}:${data.aws_caller_identity.current.account_id}:trail/hockeytrack-account"]
    }
  }
}

resource "aws_s3_bucket_policy" "trail" {
  bucket = aws_s3_bucket.trail.id
  policy = data.aws_iam_policy_document.trail_bucket.json

  # Ordering, learned the hard way on the archive: this policy denies the very
  # calls that configure versioning and lifecycle, so it must be written last.
  # Otherwise the first apply is a coin flip between a clean run and an
  # AccessDenied halfway through, with the deny live and nothing able to retry.
  depends_on = [
    aws_s3_bucket_versioning.trail,
    aws_s3_bucket_lifecycle_configuration.trail,
    aws_s3_bucket_public_access_block.trail,
    aws_s3_bucket_server_side_encryption_configuration.trail,
  ]
}

resource "aws_cloudtrail" "account" {
  name           = "hockeytrack-account"
  s3_bucket_name = aws_s3_bucket.trail.id

  # Multi-region: an attacker who creates resources in an unused region is a
  # standard move precisely because single-region trails miss it.
  is_multi_region_trail = true

  # Lets `aws cloudtrail validate-logs` prove a log file has not been altered
  # or removed since delivery. Without it the log is evidence only as long as
  # nobody with write access wanted otherwise.
  enable_log_file_validation = true

  # Note what is NOT here: data events on this trail's own log bucket, which
  # would be the only way to see individual log objects being deleted. AWS
  # advises against it, and the reason is a feedback loop rather than cost
  # alone: "when CloudTrail delivers logs, the PutObject event occurs on the S3
  # bucket. If the S3 bucket is also specified in the data events section, the
  # trail processes and logs the PutObject event as a data event. That action is
  # another PutObject event, and the trail processes and logs the event again."
  # The documented escape is RecursiveLogging=false on the trail, which the AWS
  # provider does not expose as of v5.100.0. So deletion of individual log
  # objects is covered by prevention (the MFA deny on the bucket) and recovery
  # (versioning) rather than by detection. Revisit when the provider catches up.
  #
  # Write-only data events on the raw archive. Reads are the volume driver and
  # would mainly buy exfiltration detection; writes are what would destroy the
  # one asset here that cannot be rebuilt, and they are comparatively rare.
  # Set cloudtrail_archive_read_events to widen this to All if exfiltration
  # detection becomes worth the cost.
  dynamic "event_selector" {
    for_each = var.cloudtrail_archive_data_events ? [1] : []
    content {
      read_write_type           = var.cloudtrail_archive_read_events ? "All" : "WriteOnly"
      include_management_events = true
      data_resource {
        type   = "AWS::S3::Object"
        values = ["${aws_s3_bucket.raw.arn}/"]
      }
    }
  }

  # Every region's events, in one us-east-1 log group. This exists for one
  # specific reason: console sign-in is NOT global. CloudTrail regionalises it
  # to the region behind the sign-in endpoint, and this account has real root
  # logins recorded in us-east-2 as well as us-east-1. A single-region
  # EventBridge rule would silently miss them, which for the account's most
  # privileged principal is the wrong thing to miss. A multi-region trail
  # feeding one log group catches all of them without a rule per region.
  cloud_watch_logs_group_arn = "${aws_cloudwatch_log_group.trail.arn}:*"
  cloud_watch_logs_role_arn  = aws_iam_role.cloudtrail_logs.arn

  depends_on = [aws_s3_bucket_policy.trail]
}

# Shorter than the S3 retention on purpose. S3 holds the durable, validated
# copy for a year; this group exists to be pattern-matched in near real time,
# and paying to store a second full year of it buys nothing.
resource "aws_cloudwatch_log_group" "trail" {
  name              = "/aws/cloudtrail/hockeytrack-account"
  retention_in_days = 90
}

resource "aws_iam_role" "cloudtrail_logs" {
  name = "hockeytrack-cloudtrail-logs"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "cloudtrail.amazonaws.com" }
      Action    = "sts:AssumeRole"
      Condition = {
        StringEquals = { "aws:SourceAccount" = data.aws_caller_identity.current.account_id }
      }
    }]
  })
}

resource "aws_iam_role_policy" "cloudtrail_logs" {
  role = aws_iam_role.cloudtrail_logs.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
      Resource = "${aws_cloudwatch_log_group.trail.arn}:*"
    }]
  })
}
