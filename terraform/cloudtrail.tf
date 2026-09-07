# Account-level audit log. Covers both this project and the scoreboard, which
# share this account -- the account is the trust boundary, not the repository,
# so the trail lives here in the platform stack rather than being duplicated.
#
# Deliberately its own bucket, not the raw archive: an audit log stored inside
# the blast radius it exists to describe is not an audit log. Whatever can
# destroy the archive should not also be able to erase the record of doing so.

resource "aws_s3_bucket" "trail" {
  bucket = "hockeytrack-cloudtrail-${data.aws_caller_identity.current.account_id}"
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

# Log volume on a single-user account is small, but unbounded retention is an
# unbounded bill. A year is long enough to investigate something noticed late.
resource "aws_s3_bucket_lifecycle_configuration" "trail" {
  bucket = aws_s3_bucket.trail.id
  rule {
    id     = "expire-logs"
    status = "Enabled"
    filter {}
    expiration {
      days = var.cloudtrail_retention_days
    }
  }
}

# CloudTrail writes as a service principal, so it needs explicit permission.
# Both statements are scoped by aws:SourceArn to this trail, so another
# account's trail cannot be pointed at this bucket.
data "aws_iam_policy_document" "trail_bucket" {
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
