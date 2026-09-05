resource "aws_ecr_repository" "main" {
  name                 = "hockeytrack"
  image_tag_mutability = "IMMUTABLE"
  force_delete         = true

  image_scanning_configuration {
    scan_on_push = true
  }
}

# Housekeeping so debug and superseded release images stop accumulating.
#
# Safety argument: tags are immutable and every `make deploy` pushes a fresh
# git-SHA tag that the Lambdas are then pointed at, so the deployed image is
# always among the most recently pushed. Rule 2 expires by push order and keeps
# the newest var.ecr_keep_images; the deployed tag is only at risk if more than
# that many images are pushed after it without a deploy, which never happens in
# this workflow. Rule 1 only touches untagged images, which nothing references.
resource "aws_ecr_lifecycle_policy" "main" {
  repository = aws_ecr_repository.main.name

  policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "Expire untagged images after 1 day"
        selection = {
          tagStatus   = "untagged"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = 1
        }
        action = { type = "expire" }
      },
      {
        rulePriority = 2
        description  = "Keep only the ${var.ecr_keep_images} most recently pushed tagged images"
        selection = {
          tagStatus      = "tagged"
          tagPatternList = ["*"]
          countType      = "imageCountMoreThan"
          countNumber    = var.ecr_keep_images
        }
        action = { type = "expire" }
      },
    ]
  })
}
