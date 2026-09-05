# ECR scan-on-push results land on the default event bus (not the hockeytrack
# bus). Only scans that report a CRITICAL or HIGH finding are forwarded.
resource "aws_cloudwatch_event_rule" "ecr_scan_findings" {
  name        = "hockeytrack-ecr-scan-findings"
  description = "ECR image scan completed with CRITICAL or HIGH findings"

  event_pattern = jsonencode({
    source        = ["aws.ecr"]
    "detail-type" = ["ECR Image Scan"]
    detail = {
      "repository-name" = [aws_ecr_repository.main.name]
      "scan-status"     = ["COMPLETE"]
      "finding-severity-counts" = {
        "$or" = [
          { CRITICAL = [{ numeric = [">", 0] }] },
          { HIGH = [{ numeric = [">", 0] }] },
        ]
      }
    }
  })
}

resource "aws_cloudwatch_event_target" "ecr_scan_findings_sns" {
  rule = aws_cloudwatch_event_rule.ecr_scan_findings.name
  arn  = aws_sns_topic.alerts.arn
}

# Setting a topic policy replaces the SNS default, so the account-owner
# statement is restated here: the CloudWatch alarms in alarms.tf publish
# under it.
resource "aws_sns_topic_policy" "alerts" {
  arn = aws_sns_topic.alerts.arn
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "AccountOwner"
        Effect    = "Allow"
        Principal = { AWS = "*" }
        Action = [
          "SNS:GetTopicAttributes",
          "SNS:SetTopicAttributes",
          "SNS:AddPermission",
          "SNS:RemovePermission",
          "SNS:DeleteTopic",
          "SNS:Subscribe",
          "SNS:ListSubscriptionsByTopic",
          "SNS:Publish",
        ]
        Resource  = aws_sns_topic.alerts.arn
        Condition = { StringEquals = { "AWS:SourceOwner" = data.aws_caller_identity.current.account_id } }
      },
      {
        Sid       = "EcrScanFindings"
        Effect    = "Allow"
        Principal = { Service = "events.amazonaws.com" }
        Action    = "SNS:Publish"
        Resource  = aws_sns_topic.alerts.arn
        Condition = {
          ArnEquals = { "aws:SourceArn" = aws_cloudwatch_event_rule.ecr_scan_findings.arn }
        }
      },
    ]
  })
}
