# The query API: api.hockeytrack.davidjdrake.com
#
# Two more modes of the one ingestor image. `api` answers HTTP from the raw
# bucket, the index bucket and the site's schedule document; `indexer`
# derives the index (one summary per finished game, one rollup per season)
# from the raw archive after every nhl.game.final event and once a day.
# Design: docs/superpowers/specs/2026-10-11-query-api-design.md.
#
# The API function has a Function URL that only CloudFront may invoke: the
# URL is in AWS_IAM mode and the distribution signs its origin requests
# through an origin access control, so there is no path to the function
# that skips the edge cache. Both functions are read-only against the raw
# archive; the indexer writes only to the index bucket, which holds nothing
# that cannot be rebuilt from raw/ with `make index`.

variable "api_domain" {
  type    = string
  default = "api.hockeytrack.davidjdrake.com"
}

# The honest risk of a public API in this account is concurrency, not data:
# the Lambda concurrency pool is shared with the pollers, and a traffic
# spike could starve them mid-game. -1 leaves the function unreserved,
# which is the only option while the account's quota is the default 10
# (AWS requires 10 unreserved). Once the quota is raised, set a cap here
# so the API can never take more than its share.
variable "api_reserved_concurrency" {
  type        = number
  default     = -1
  description = "Reserved concurrency for hockeytrack-api; -1 for none. Set once the account's Lambda quota is above the AWS-required 10 unreserved executions."

  validation {
    condition     = var.api_reserved_concurrency == -1 || var.api_reserved_concurrency >= 1
    error_message = "api_reserved_concurrency must be -1 (unreserved) or a positive whole number."
  }
}

# ---- The index bucket ----
#
# Derived data lives apart from the raw archive on purpose: raw/ is
# write-once, versioned, size-alarmed and MFA-gated, and a rollup that is
# rewritten after every game would churn all four. This bucket is private,
# unversioned and disposable.
resource "aws_s3_bucket" "index" {
  bucket = "hockeytrack-index-${data.aws_caller_identity.current.account_id}"
}

resource "aws_s3_bucket_public_access_block" "index" {
  bucket                  = aws_s3_bucket.index.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# ---- Roles ----

resource "aws_iam_role" "api" {
  name               = "hockeytrack-api"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume.json
}

# Reads only. The API never touches DynamoDB, the bus, Scheduler or
# another function.
data "aws_iam_policy_document" "api" {
  statement {
    actions   = local.logs_actions
    resources = [local.lambda_log_group_arn["hockeytrack-api"]]
  }
  statement {
    actions = ["s3:GetObject"]
    resources = [
      "${aws_s3_bucket.raw.arn}/raw/*",
      "${aws_s3_bucket.index.arn}/v1/*",
      "${aws_s3_bucket.site.arn}/data/*",
    ]
  }
  statement {
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.raw.arn, aws_s3_bucket.index.arn]
  }
}

resource "aws_iam_role_policy" "api" {
  role   = aws_iam_role.api.id
  policy = data.aws_iam_policy_document.api.json
}

resource "aws_iam_role" "indexer" {
  name               = "hockeytrack-indexer"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume.json
}

data "aws_iam_policy_document" "indexer" {
  statement {
    actions   = local.logs_actions
    resources = [local.lambda_log_group_arn["hockeytrack-indexer"]]
  }
  statement {
    actions   = ["s3:GetObject"]
    resources = ["${aws_s3_bucket.raw.arn}/raw/*"]
  }
  statement {
    actions   = ["s3:GetObject", "s3:PutObject"]
    resources = ["${aws_s3_bucket.index.arn}/v1/*"]
  }
  statement {
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.raw.arn, aws_s3_bucket.index.arn]
  }
  statement {
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.dlq["indexer"].arn]
  }
}

resource "aws_iam_role_policy" "indexer" {
  role   = aws_iam_role.indexer.id
  policy = data.aws_iam_policy_document.indexer.json
}

# ---- Functions ----

resource "aws_lambda_function" "api" {
  function_name = "hockeytrack-api"
  package_type  = "Image"
  image_uri     = local.image_uri
  role          = aws_iam_role.api.arn
  architectures = ["x86_64"]
  # A player's game log reads ~80 summaries concurrently; everything else
  # is one or two objects. The rollup is cached in memory between requests.
  timeout     = 30
  memory_size = 512

  reserved_concurrent_executions = var.api_reserved_concurrency

  environment {
    variables = {
      MODE         = "api"
      RAW_BUCKET   = aws_s3_bucket.raw.bucket
      INDEX_BUCKET = aws_s3_bucket.index.bucket
      SITE_BUCKET  = aws_s3_bucket.site.bucket
      IMAGE_TAG    = var.image_tag
    }
  }
}

resource "aws_lambda_function_url" "api" {
  function_name      = aws_lambda_function.api.function_name
  authorization_type = "AWS_IAM"
  invoke_mode        = "BUFFERED"
}

# Only the distribution may call the URL. CloudFront signs each origin
# request with SigV4 through the origin access control below, and this
# permission is what makes that signature mean something.
resource "aws_lambda_permission" "api_cloudfront" {
  statement_id           = "AllowCloudFrontInvokeFunctionURL"
  action                 = "lambda:InvokeFunctionUrl"
  function_name          = aws_lambda_function.api.function_name
  principal              = "cloudfront.amazonaws.com"
  source_arn             = aws_cloudfront_distribution.api.arn
  function_url_auth_type = "AWS_IAM"
}

# Function URLs created since October 2025 also need lambda:InvokeFunction;
# with only the statement above, every request CloudFront signs is refused
# with a 403 before the function runs. invoked_via_function_url scopes this
# grant to calls that arrive through the URL, so the distribution still
# cannot invoke the function any other way.
resource "aws_lambda_permission" "api_cloudfront_invoke" {
  statement_id             = "AllowCloudFrontInvokeFunctionViaURL"
  action                   = "lambda:InvokeFunction"
  function_name            = aws_lambda_function.api.function_name
  principal                = "cloudfront.amazonaws.com"
  source_arn               = aws_cloudfront_distribution.api.arn
  invoked_via_function_url = true
}

resource "aws_lambda_function" "indexer" {
  function_name = "hockeytrack-indexer"
  package_type  = "Image"
  image_uri     = local.image_uri
  role          = aws_iam_role.indexer.arn
  architectures = ["x86_64"]
  # The daily sync lists a whole season's prefix, live snapshots included,
  # and a rollup reads every summary of the season (~1,400 small objects).
  timeout     = 600
  memory_size = 1024

  environment {
    variables = {
      MODE         = "indexer"
      RAW_BUCKET   = aws_s3_bucket.raw.bucket
      INDEX_BUCKET = aws_s3_bucket.index.bucket
    }
  }

  dead_letter_config {
    target_arn = aws_sqs_queue.dlq["indexer"].arn
  }
}

# ---- Triggers: every final, and once a day ----

# The README's own extension pattern: a rule on nhl.game.final hands the
# game's prefix to a function. Statistics are current within a minute of
# the final horn.
resource "aws_cloudwatch_event_rule" "game_final_index" {
  name           = "hockeytrack-game-final-index"
  description    = "Index a game's summary and its season's rollup when the poller publishes the final"
  event_bus_name = aws_cloudwatch_event_bus.main.name
  event_pattern = jsonencode({
    "source"      = ["hockeytrack.poller"]
    "detail-type" = ["nhl.game.final"]
  })
}

resource "aws_cloudwatch_event_target" "game_final_index" {
  rule           = aws_cloudwatch_event_rule.game_final_index.name
  event_bus_name = aws_cloudwatch_event_bus.main.name
  target_id      = "indexer"
  arn            = aws_lambda_function.indexer.arn
}

resource "aws_lambda_permission" "indexer_events" {
  statement_id  = "AllowGameFinalRule"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.indexer.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.game_final_index.arn
}

resource "aws_iam_role" "scheduler_invoke_indexer" {
  name               = "hockeytrack-scheduler-invoke-indexer"
  assume_role_policy = data.aws_iam_policy_document.scheduler_assume.json
}

resource "aws_iam_role_policy" "scheduler_invoke_indexer" {
  role = aws_iam_role.scheduler_invoke_indexer.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = "lambda:InvokeFunction"
      Resource = aws_lambda_function.indexer.arn
    }]
  })
}

# Daily at 10:00 UTC, an hour after schedule-sync: build any summary the
# current season is missing (a lost event, a race between two finals, a
# backfill) and rebuild the rollup. season 0 means "the current season".
resource "aws_scheduler_schedule" "daily_index" {
  name                = "hockeytrack-daily-index"
  group_name          = aws_scheduler_schedule_group.games.name
  schedule_expression = "cron(0 10 * * ? *)"

  flexible_time_window {
    mode = "OFF"
  }

  target {
    arn      = aws_lambda_function.indexer.arn
    role_arn = aws_iam_role.scheduler_invoke_indexer.arn
    input    = jsonencode({ season = 0 })
  }
}

# ---- The edge: certificate, DNS, CloudFront ----

resource "aws_acm_certificate" "api" {
  domain_name       = var.api_domain
  validation_method = "DNS"
  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_route53_record" "api_cert_validation" {
  for_each = {
    for dvo in aws_acm_certificate.api.domain_validation_options : dvo.domain_name => {
      name   = dvo.resource_record_name
      record = dvo.resource_record_value
      type   = dvo.resource_record_type
    }
  }
  zone_id         = data.aws_route53_zone.site.zone_id
  name            = each.value.name
  type            = each.value.type
  records         = [each.value.record]
  ttl             = 60
  allow_overwrite = true
}

resource "aws_acm_certificate_validation" "api" {
  certificate_arn         = aws_acm_certificate.api.arn
  validation_record_fqdns = [for r in aws_route53_record.api_cert_validation : r.fqdn]
}

resource "aws_cloudfront_origin_access_control" "api" {
  name                              = "hockeytrack-api"
  origin_access_control_origin_type = "lambda"
  signing_behavior                  = "always"
  signing_protocol                  = "sigv4"
}

# The cache key is the path and the whole query string, nothing else; the
# lifetime is whatever the function says in Cache-Control (a day for a
# finished game, ten seconds for a live one, five minutes for the index),
# with a minute's default and a day's ceiling.
resource "aws_cloudfront_cache_policy" "api" {
  name        = "hockeytrack-api"
  default_ttl = 60
  max_ttl     = 86400
  min_ttl     = 0
  parameters_in_cache_key_and_forwarded_to_origin {
    cookies_config {
      cookie_behavior = "none"
    }
    headers_config {
      header_behavior = "none"
    }
    query_strings_config {
      query_string_behavior = "all"
    }
    enable_accept_encoding_gzip   = true
    enable_accept_encoding_brotli = true
  }
}

# A JSON origin needs no script, style or frame of any kind.
resource "aws_cloudfront_response_headers_policy" "api" {
  name = "hockeytrack-api-security"

  security_headers_config {
    strict_transport_security {
      access_control_max_age_sec = 31536000
      include_subdomains         = true
      override                   = true
    }
    content_type_options {
      override = true
    }
    referrer_policy {
      referrer_policy = "no-referrer"
      override        = true
    }
    frame_options {
      frame_option = "DENY"
      override     = true
    }
    content_security_policy {
      content_security_policy = "default-src 'none'; frame-ancestors 'none'"
      override                = true
    }
  }
}

resource "aws_cloudfront_distribution" "api" {
  enabled         = true
  is_ipv6_enabled = true
  http_version    = "http2and3"
  aliases         = [var.api_domain]
  price_class     = "PriceClass_100"
  comment         = var.api_domain

  origin {
    # The Function URL host, without scheme or trailing slash.
    domain_name              = trimsuffix(trimprefix(aws_lambda_function_url.api.function_url, "https://"), "/")
    origin_id                = "api-lambda"
    origin_access_control_id = aws_cloudfront_origin_access_control.api.id
    custom_origin_config {
      http_port              = 80
      https_port             = 443
      origin_protocol_policy = "https-only"
      origin_ssl_protocols   = ["TLSv1.2"]
    }
  }

  default_cache_behavior {
    target_origin_id           = "api-lambda"
    viewer_protocol_policy     = "redirect-to-https"
    allowed_methods            = ["GET", "HEAD"]
    cached_methods             = ["GET", "HEAD"]
    compress                   = true
    cache_policy_id            = aws_cloudfront_cache_policy.api.id
    response_headers_policy_id = aws_cloudfront_response_headers_policy.api.id
  }

  restrictions {
    geo_restriction {
      restriction_type = "none"
    }
  }

  viewer_certificate {
    acm_certificate_arn      = aws_acm_certificate_validation.api.certificate_arn
    ssl_support_method       = "sni-only"
    minimum_protocol_version = "TLSv1.2_2021"
  }
}

resource "aws_route53_record" "api_a" {
  zone_id = data.aws_route53_zone.site.zone_id
  name    = var.api_domain
  type    = "A"
  alias {
    name                   = aws_cloudfront_distribution.api.domain_name
    zone_id                = aws_cloudfront_distribution.api.hosted_zone_id
    evaluate_target_health = false
  }
}

resource "aws_route53_record" "api_aaaa" {
  zone_id = data.aws_route53_zone.site.zone_id
  name    = var.api_domain
  type    = "AAAA"
  alias {
    name                   = aws_cloudfront_distribution.api.domain_name
    zone_id                = aws_cloudfront_distribution.api.hosted_zone_id
    evaluate_target_health = false
  }
}

# ---- Alarms ----

resource "aws_cloudwatch_metric_alarm" "api_errors" {
  alarm_name          = "hockeytrack-api-errors"
  alarm_description   = "hockeytrack-api is failing requests: check its log group for the key it could not read"
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  dimensions          = { FunctionName = aws_lambda_function.api.function_name }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 3
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
}

resource "aws_cloudwatch_metric_alarm" "indexer_errors" {
  alarm_name          = "hockeytrack-indexer-errors"
  alarm_description   = "hockeytrack-indexer failed: statistics will lag until the daily sync; check its log group"
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  dimensions          = { FunctionName = aws_lambda_function.indexer.function_name }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
}

output "api_url" {
  value = "https://${var.api_domain}/v1"
}

output "index_bucket" {
  value = aws_s3_bucket.index.bucket
}
