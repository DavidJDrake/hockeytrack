resource "aws_cloudwatch_metric_alarm" "dlq_depth" {
  for_each            = aws_sqs_queue.dlq
  alarm_name          = "hockeytrack-${each.key}-dlq-depth"
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  dimensions          = { QueueName = each.value.name }
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  alarm_actions       = [aws_sns_topic.alerts.arn]
}

resource "aws_cloudwatch_metric_alarm" "poller_errors" {
  alarm_name          = "hockeytrack-poller-errors"
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  dimensions          = { FunctionName = aws_lambda_function.poller.function_name }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 3
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
}

# The sweeper is the heartbeat of the Scheduler → Lambda path: it must run
# every 5 minutes whether or not games are on. If Scheduler cannot assume its
# execution role (as happened 2026-09-04/05) the failure is silent — no Lambda
# error, no DLQ message, just no invocations. Missing data therefore counts as
# breaching. Two 15-minute windows with fewer than one invocation each
# (expected: three) fire the alarm, i.e. ~30 minutes of silence.
resource "aws_cloudwatch_metric_alarm" "sweeper_silent" {
  alarm_name          = "hockeytrack-sweeper-silent"
  alarm_description   = "No hockeytrack-sweeper invocations for 30 minutes: EventBridge Scheduler is not invoking Lambdas (check the scheduler roles' trust policies and the hockeytrack-games schedule group)"
  namespace           = "AWS/Lambda"
  metric_name         = "Invocations"
  dimensions          = { FunctionName = aws_lambda_function.sweeper.function_name }
  statistic           = "Sum"
  period              = 900
  evaluation_periods  = 2
  datapoints_to_alarm = 2
  threshold           = 1
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "breaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]
}
