data "aws_iam_policy_document" "lambda_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }
}

# Confused-deputy guard: only schedules in our account's games group may assume these roles.
data "aws_iam_policy_document" "scheduler_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["scheduler.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
    # Scheduler presents the schedule GROUP's ARN as aws:SourceArn (not the
    # individual schedule's), so the condition must match schedule-group/<name>.
    # A schedule/<group>/* pattern here silently denied every invocation.
    condition {
      test     = "ArnEquals"
      variable = "aws:SourceArn"
      values   = [aws_scheduler_schedule_group.games.arn]
    }
  }
}

# Each Lambda may only write its own log group. Function names are literal here
# rather than aws_lambda_function.*.function_name to avoid a cycle (the functions
# depend on these roles).
locals {
  logs_actions = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"]
  lambda_log_group_arn = {
    for name in ["hockeytrack-schedule-sync", "hockeytrack-poller", "hockeytrack-sweeper"] :
    name => "arn:aws:logs:${var.region}:${data.aws_caller_identity.current.account_id}:log-group:/aws/lambda/${name}:*"
  }
}

# ---- schedule-sync ----
resource "aws_iam_role" "schedule_sync" {
  name               = "hockeytrack-schedule-sync"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume.json
}

data "aws_iam_policy_document" "schedule_sync" {
  statement {
    actions   = local.logs_actions
    resources = [local.lambda_log_group_arn["hockeytrack-schedule-sync"]]
  }
  statement {
    actions   = ["dynamodb:UpdateItem", "dynamodb:GetItem", "dynamodb:Query"]
    resources = [aws_dynamodb_table.games.arn, "${aws_dynamodb_table.games.arn}/index/*"]
  }
  statement {
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.raw.arn}/raw/schedule/*", "${aws_s3_bucket.raw.arn}/raw/standings/*", "${aws_s3_bucket.site.arn}/data/*"]
  }
  statement {
    actions   = ["scheduler:CreateSchedule", "scheduler:UpdateSchedule", "scheduler:DeleteSchedule", "scheduler:GetSchedule"]
    resources = ["arn:aws:scheduler:${var.region}:${data.aws_caller_identity.current.account_id}:schedule/${aws_scheduler_schedule_group.games.name}/*"]
  }
  statement {
    actions   = ["iam:PassRole"]
    resources = [aws_iam_role.scheduler_invoke.arn]
  }
  statement {
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.dlq["schedule-sync"].arn]
  }
}

resource "aws_iam_role_policy" "schedule_sync" {
  role   = aws_iam_role.schedule_sync.id
  policy = data.aws_iam_policy_document.schedule_sync.json
}

# ---- poller ----
resource "aws_iam_role" "poller" {
  name               = "hockeytrack-poller"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume.json
}

data "aws_iam_policy_document" "poller" {
  statement {
    actions   = local.logs_actions
    resources = [local.lambda_log_group_arn["hockeytrack-poller"]]
  }
  statement {
    actions   = ["dynamodb:UpdateItem", "dynamodb:GetItem", "dynamodb:Query"]
    resources = [aws_dynamodb_table.games.arn, "${aws_dynamodb_table.games.arn}/index/*"]
  }
  statement {
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.raw.arn}/raw/*"]
  }
  statement {
    actions   = ["events:PutEvents"]
    resources = [aws_cloudwatch_event_bus.main.arn]
  }
  statement {
    actions   = ["lambda:InvokeFunction"]
    resources = ["arn:aws:lambda:${var.region}:${data.aws_caller_identity.current.account_id}:function:hockeytrack-poller"]
  }
  statement {
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.dlq["poller"].arn]
  }
}

resource "aws_iam_role_policy" "poller" {
  role   = aws_iam_role.poller.id
  policy = data.aws_iam_policy_document.poller.json
}

# ---- sweeper ----
resource "aws_iam_role" "sweeper" {
  name               = "hockeytrack-sweeper"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume.json
}

data "aws_iam_policy_document" "sweeper" {
  statement {
    actions   = local.logs_actions
    resources = [local.lambda_log_group_arn["hockeytrack-sweeper"]]
  }
  statement {
    actions   = ["dynamodb:GetItem", "dynamodb:Query"]
    resources = [aws_dynamodb_table.games.arn, "${aws_dynamodb_table.games.arn}/index/*"]
  }
  statement {
    actions   = ["lambda:InvokeFunction"]
    resources = [aws_lambda_function.poller.arn]
  }
  statement {
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.dlq["sweeper"].arn]
  }
}

resource "aws_iam_role_policy" "sweeper" {
  role   = aws_iam_role.sweeper.id
  policy = data.aws_iam_policy_document.sweeper.json
}

# ---- roles EventBridge Scheduler assumes ----
resource "aws_iam_role" "scheduler_invoke" {
  name               = "hockeytrack-scheduler-invoke"
  assume_role_policy = data.aws_iam_policy_document.scheduler_assume.json
}

resource "aws_iam_role_policy" "scheduler_invoke" {
  role = aws_iam_role.scheduler_invoke.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = "lambda:InvokeFunction"
      Resource = "arn:aws:lambda:${var.region}:${data.aws_caller_identity.current.account_id}:function:hockeytrack-poller"
    }]
  })
}

resource "aws_iam_role" "scheduler_invoke_sync" {
  name               = "hockeytrack-scheduler-invoke-sync"
  assume_role_policy = data.aws_iam_policy_document.scheduler_assume.json
}

resource "aws_iam_role_policy" "scheduler_invoke_sync" {
  role = aws_iam_role.scheduler_invoke_sync.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = "lambda:InvokeFunction"
      Resource = aws_lambda_function.schedule_sync.arn
    }]
  })
}

resource "aws_iam_role" "scheduler_invoke_sweeper" {
  name               = "hockeytrack-scheduler-invoke-sweeper"
  assume_role_policy = data.aws_iam_policy_document.scheduler_assume.json
}

resource "aws_iam_role_policy" "scheduler_invoke_sweeper" {
  role = aws_iam_role.scheduler_invoke_sweeper.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = "lambda:InvokeFunction"
      Resource = aws_lambda_function.sweeper.arn
    }]
  })
}
