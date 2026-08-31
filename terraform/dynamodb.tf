resource "aws_dynamodb_table" "games" {
  name         = "hockeytrack-games"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "gameId"

  attribute {
    name = "gameId"
    type = "N"
  }

  attribute {
    name = "gameDate"
    type = "S"
  }

  global_secondary_index {
    name            = "byGameDate"
    hash_key        = "gameDate"
    projection_type = "ALL"
  }
}
