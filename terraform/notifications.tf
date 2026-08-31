# Example consumer: notify on every Tampa Bay Lightning goal.
# Demonstrates the extension pattern — a rule + target, zero pipeline changes.

variable "lightning_email" {
  type        = string
  default     = ""
  description = "Email to notify on every TBL goal; empty skips the subscription"
}

resource "aws_sns_topic" "tbl_goals" {
  name = "hockeytrack-tbl-goals"
}

resource "aws_sns_topic_policy" "tbl_goals" {
  arn = aws_sns_topic.tbl_goals.arn
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "events.amazonaws.com" }
      Action    = "sns:Publish"
      Resource  = aws_sns_topic.tbl_goals.arn
      Condition = {
        ArnEquals = { "aws:SourceArn" = aws_cloudwatch_event_rule.tbl_goals.arn }
      }
    }]
  })
}

resource "aws_cloudwatch_event_rule" "tbl_goals" {
  name           = "hockeytrack-tbl-goals"
  event_bus_name = aws_cloudwatch_event_bus.main.name

  event_pattern = jsonencode({
    source        = ["hockeytrack.poller"]
    "detail-type" = ["nhl.game.play"]
    detail = {
      playType    = ["goal"]
      scoringTeam = ["TBL"]
    }
  })
}

resource "aws_cloudwatch_event_target" "tbl_goals_sns" {
  rule           = aws_cloudwatch_event_rule.tbl_goals.name
  event_bus_name = aws_cloudwatch_event_bus.main.name
  arn            = aws_sns_topic.tbl_goals.arn

  input_transformer {
    input_paths = {
      away   = "$.detail.awayTeam"
      home   = "$.detail.homeTeam"
      period = "$.detail.period"
      clock  = "$.detail.timeInPeriod"
      tbl    = "$.detail.score.TBL"
    }
    input_template = "\"GOAL Lightning! <away> @ <home> — TBL now has <tbl>, period <period> at <clock>.\""
  }
}

resource "aws_sns_topic_subscription" "tbl_goals_email" {
  count     = var.lightning_email == "" ? 0 : 1
  topic_arn = aws_sns_topic.tbl_goals.arn
  protocol  = "email"
  endpoint  = var.lightning_email
}
