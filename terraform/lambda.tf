locals {
  image_uri = "${aws_ecr_repository.main.repository_url}:${var.image_tag}"
  common_env = {
    GAMES_TABLE = aws_dynamodb_table.games.name
    RAW_BUCKET  = aws_s3_bucket.raw.bucket
    EVENT_BUS   = aws_cloudwatch_event_bus.main.name
  }
}

resource "aws_lambda_function" "schedule_sync" {
  function_name = "hockeytrack-schedule-sync"
  package_type  = "Image"
  image_uri     = local.image_uri
  role          = aws_iam_role.schedule_sync.arn
  architectures = ["x86_64"]
  # Full-season sync upserts ~1,400 games and scheduler entries per run.
  timeout       = 600
  memory_size   = 256

  environment {
    variables = merge(local.common_env, {
      MODE                = "schedule-sync"
      SCHEDULER_GROUP     = aws_scheduler_schedule_group.games.name
      POLLER_FUNCTION_ARN = aws_lambda_function.poller.arn
      SCHEDULER_ROLE_ARN  = aws_iam_role.scheduler_invoke.arn
      SITE_BUCKET         = aws_s3_bucket.site.bucket
    })
  }

  dead_letter_config {
    target_arn = aws_sqs_queue.dlq["schedule-sync"].arn
  }
}

resource "aws_lambda_function" "poller" {
  function_name = "hockeytrack-poller"
  package_type  = "Image"
  image_uri     = local.image_uri
  role          = aws_iam_role.poller.arn
  architectures = ["x86_64"]
  timeout       = 900
  memory_size   = 256
  # No reserved concurrency: the account's default 10-execution limit cannot
  # spare a reservation (AWS requires 10 unreserved). The DynamoDB lease
  # already guarantees one chain per game. Request a quota increase before
  # the season if >10 games may poll simultaneously.

  environment {
    variables = merge(local.common_env, {
      MODE                 = "poller"
      POLLER_FUNCTION_NAME = "hockeytrack-poller"
    })
  }

  dead_letter_config {
    target_arn = aws_sqs_queue.dlq["poller"].arn
  }
}

resource "aws_lambda_function" "sweeper" {
  function_name = "hockeytrack-sweeper"
  package_type  = "Image"
  image_uri     = local.image_uri
  role          = aws_iam_role.sweeper.arn
  architectures = ["x86_64"]
  timeout       = 60
  memory_size   = 256

  environment {
    variables = merge(local.common_env, {
      MODE                 = "sweeper"
      POLLER_FUNCTION_NAME = aws_lambda_function.poller.function_name
    })
  }

  dead_letter_config {
    target_arn = aws_sqs_queue.dlq["sweeper"].arn
  }
}
