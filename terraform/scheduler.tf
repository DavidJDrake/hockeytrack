resource "aws_scheduler_schedule_group" "games" {
  name = "hockeytrack-games"
}

# Daily schedule-sync at 09:00 UTC (~4am ET).
resource "aws_scheduler_schedule" "daily_sync" {
  name                = "hockeytrack-daily-sync"
  group_name          = aws_scheduler_schedule_group.games.name
  schedule_expression = "cron(0 9 * * ? *)"

  flexible_time_window {
    mode = "OFF"
  }

  target {
    arn      = aws_lambda_function.schedule_sync.arn
    role_arn = aws_iam_role.scheduler_invoke_sync.arn
  }
}

# Sweeper every 5 minutes.
resource "aws_scheduler_schedule" "sweeper" {
  name                = "hockeytrack-sweeper"
  group_name          = aws_scheduler_schedule_group.games.name
  schedule_expression = "rate(5 minutes)"

  flexible_time_window {
    mode = "OFF"
  }

  target {
    arn      = aws_lambda_function.sweeper.arn
    role_arn = aws_iam_role.scheduler_invoke_sweeper.arn
  }
}
