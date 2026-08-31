resource "aws_ecr_repository" "main" {
  name                 = "hockeytrack"
  image_tag_mutability = "IMMUTABLE"
  force_delete         = true
}
