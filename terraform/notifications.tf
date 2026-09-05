# Example consumers: per-team alerts delivered by email.
# Demonstrates the extension pattern — rules + targets, zero pipeline changes.
#
# One SNS topic and one email subscription per team; each opted-in alert type
# is its own EventBridge rule fanning into that team's topic. Add a team by
# adding a key to var.team_alerts — nothing else changes.

# No default on purpose: a plan without terraform.tfvars must fail rather than
# silently destroy the subscriptions. Use an empty email to opt out explicitly.
variable "team_alerts" {
  type = map(object({
    email      = string                # empty string skips the subscription
    name       = optional(string)      # display name in messages; defaults to the key
    goals      = optional(bool, true)  # every goal the team scores
    game_start = optional(bool, false) # puck drop (nhl.game.status -> LIVE)
    final      = optional(bool, false) # final score (nhl.game.final)
    overtime   = optional(bool, false) # OT / shootout starting (nhl.game.play period-start, period > 3)
  }))
  description = "Per-team alert config keyed by NHL team abbreviation (e.g. TBL); each team gets an SNS topic, one rule per enabled alert type, and an email subscription"

  validation {
    condition     = alltrue([for k in keys(var.team_alerts) : can(regex("^[A-Z]{3}$", k))])
    error_message = "team_alerts keys must be 3-letter uppercase NHL abbreviations (e.g. TBL)."
  }

  validation {
    condition     = alltrue([for k, v in var.team_alerts : v.goals || v.game_start || v.final || v.overtime])
    error_message = "Every team_alerts entry must enable at least one alert type (goals, game_start, final, overtime); otherwise its topic policy would have no source rules."
  }
}

locals {
  team_display = { for k, v in var.team_alerts : k => coalesce(v.name, k) }

  # Team filter for events that carry both sides: TBL is home or away.
  team_side_filter = {
    for k in keys(var.team_alerts) : k => [{ homeTeam = [k] }, { awayTeam = [k] }]
  }
}

# Topic names keep the original "-goals" suffix even though they now carry every
# alert type: renaming an SNS topic recreates it and its subscriptions, which
# would force each subscriber to re-confirm by email.
resource "aws_sns_topic" "team" {
  for_each = var.team_alerts
  name     = "hockeytrack-${lower(each.key)}-goals"
}

resource "aws_sns_topic_policy" "team" {
  for_each = var.team_alerts
  arn      = aws_sns_topic.team[each.key].arn
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "events.amazonaws.com" }
      Action    = "sns:Publish"
      Resource  = aws_sns_topic.team[each.key].arn
      Condition = {
        ArnEquals = {
          "aws:SourceArn" = [
            for rules in [
              aws_cloudwatch_event_rule.goals,
              aws_cloudwatch_event_rule.game_start,
              aws_cloudwatch_event_rule.final,
              aws_cloudwatch_event_rule.overtime,
            ] : rules[each.key].arn if contains(keys(rules), each.key)
          ]
        }
      }
    }]
  })
}

resource "aws_sns_topic_subscription" "team_email" {
  for_each  = { for k, v in var.team_alerts : k => v if v.email != "" }
  topic_arn = aws_sns_topic.team[each.key].arn
  protocol  = "email"
  endpoint  = each.value.email
}

# --- goals -------------------------------------------------------------------

resource "aws_cloudwatch_event_rule" "goals" {
  for_each       = { for k, v in var.team_alerts : k => v if v.goals }
  name           = "hockeytrack-${lower(each.key)}-goals"
  event_bus_name = aws_cloudwatch_event_bus.main.name

  event_pattern = jsonencode({
    source        = ["hockeytrack.poller"]
    "detail-type" = ["nhl.game.play"]
    detail = {
      playType    = ["goal"]
      scoringTeam = [each.key]
    }
  })
}

resource "aws_cloudwatch_event_target" "goals_sns" {
  for_each       = aws_cloudwatch_event_rule.goals
  rule           = each.value.name
  event_bus_name = aws_cloudwatch_event_bus.main.name
  arn            = aws_sns_topic.team[each.key].arn

  input_transformer {
    input_paths = {
      away   = "$.detail.awayTeam"
      home   = "$.detail.homeTeam"
      period = "$.detail.period"
      clock  = "$.detail.timeInPeriod"
      team   = "$.detail.score.${each.key}"
    }
    input_template = jsonencode("GOAL ${local.team_display[each.key]}! <away> @ <home> — ${each.key} now has <team>, period <period> at <clock>.")
  }
}

# --- game start --------------------------------------------------------------
# nhl.game.status carries no homeTeam/awayTeam, only the score map keyed by
# abbreviation, so the team filter is "score has a <team> key". prevState is
# excluded for LIVE/CRIT so a CRIT -> LIVE bounce (e.g. into overtime) does not
# re-announce puck drop.

resource "aws_cloudwatch_event_rule" "game_start" {
  for_each       = { for k, v in var.team_alerts : k => v if v.game_start }
  name           = "hockeytrack-${lower(each.key)}-game-start"
  event_bus_name = aws_cloudwatch_event_bus.main.name

  event_pattern = jsonencode({
    source        = ["hockeytrack.poller"]
    "detail-type" = ["nhl.game.status"]
    detail = {
      gameState = ["LIVE"]
      prevState = [{ "anything-but" = ["LIVE", "CRIT"] }]
      score     = { (each.key) = [{ exists = true }] }
    }
  })
}

resource "aws_cloudwatch_event_target" "game_start_sns" {
  for_each       = aws_cloudwatch_event_rule.game_start
  rule           = each.value.name
  event_bus_name = aws_cloudwatch_event_bus.main.name
  arn            = aws_sns_topic.team[each.key].arn

  input_transformer {
    input_paths = {
      gameId = "$.detail.gameId"
    }
    input_template = jsonencode("Puck drop! ${local.team_display[each.key]} are under way — game <gameId>.")
  }
}

# --- final score -------------------------------------------------------------

resource "aws_cloudwatch_event_rule" "final" {
  for_each       = { for k, v in var.team_alerts : k => v if v.final }
  name           = "hockeytrack-${lower(each.key)}-final"
  event_bus_name = aws_cloudwatch_event_bus.main.name

  event_pattern = jsonencode({
    source        = ["hockeytrack.poller"]
    "detail-type" = ["nhl.game.final"]
    detail = {
      "$or" = local.team_side_filter[each.key]
    }
  })
}

resource "aws_cloudwatch_event_target" "final_sns" {
  for_each       = aws_cloudwatch_event_rule.final
  rule           = each.value.name
  event_bus_name = aws_cloudwatch_event_bus.main.name
  arn            = aws_sns_topic.team[each.key].arn

  input_transformer {
    input_paths = {
      away  = "$.detail.awayTeam"
      home  = "$.detail.homeTeam"
      score = "$.detail.score"
    }
    input_template = jsonencode("FINAL: <away> @ <home> — <score>.")
  }
}

# --- overtime / shootout -----------------------------------------------------
# Matched on the typed contract only: a period-start play with period > 3
# (regular season: 4 = OT, 5 = shootout; playoffs: 4+ = OT periods). The
# OT/SO label in the message comes from the raw NHL play's periodDescriptor,
# which the poller itself reads for the period number.

resource "aws_cloudwatch_event_rule" "overtime" {
  for_each       = { for k, v in var.team_alerts : k => v if v.overtime }
  name           = "hockeytrack-${lower(each.key)}-overtime"
  event_bus_name = aws_cloudwatch_event_bus.main.name

  event_pattern = jsonencode({
    source        = ["hockeytrack.poller"]
    "detail-type" = ["nhl.game.play"]
    detail = {
      playType = ["period-start"]
      period   = [{ numeric = [">", 3] }]
      "$or"    = local.team_side_filter[each.key]
    }
  })
}

resource "aws_cloudwatch_event_target" "overtime_sns" {
  for_each       = aws_cloudwatch_event_rule.overtime
  rule           = each.value.name
  event_bus_name = aws_cloudwatch_event_bus.main.name
  arn            = aws_sns_topic.team[each.key].arn

  input_transformer {
    input_paths = {
      away   = "$.detail.awayTeam"
      home   = "$.detail.homeTeam"
      period = "$.detail.period"
      ptype  = "$.detail.raw.periodDescriptor.periodType"
      score  = "$.detail.score"
    }
    input_template = jsonencode("<ptype> under way: <away> @ <home> tied <score>, period <period>.")
  }
}

# Not implemented here: a "<team> are playing tonight" morning digest. That
# needs schedule-sync to publish a per-day event (or write a summary somewhere
# a rule can see), i.e. Go changes, not just a rule. Tracked under HOC-43.

# --- state migration (HOC-43) --------------------------------------------------
# The original hard-wired TBL resources move to their for_each addresses so
# the confirmed email subscription is preserved instead of being recreated.

moved {
  from = aws_sns_topic.tbl_goals
  to   = aws_sns_topic.team["TBL"]
}

moved {
  from = aws_sns_topic_policy.tbl_goals
  to   = aws_sns_topic_policy.team["TBL"]
}

moved {
  from = aws_cloudwatch_event_rule.tbl_goals
  to   = aws_cloudwatch_event_rule.goals["TBL"]
}

moved {
  from = aws_cloudwatch_event_target.tbl_goals_sns
  to   = aws_cloudwatch_event_target.goals_sns["TBL"]
}

moved {
  from = aws_sns_topic_subscription.tbl_goals_email[0]
  to   = aws_sns_topic_subscription.team_email["TBL"]
}
