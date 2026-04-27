###############################################################################
# VPC
###############################################################################

resource "aws_vpc" "bench" {
  cidr_block           = "10.0.0.0/16"
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = { Name = "k0smotron-bench-vpc" }
}

###############################################################################
# Single public subnet — all instances land here (same AZ, no cross-AZ skew)
###############################################################################

resource "aws_subnet" "bench" {
  vpc_id                  = aws_vpc.bench.id
  cidr_block              = "10.0.1.0/24"
  availability_zone       = var.az
  map_public_ip_on_launch = true

  tags = { Name = "k0smotron-bench-subnet" }
}

###############################################################################
# Internet gateway + routing
###############################################################################

resource "aws_internet_gateway" "bench" {
  vpc_id = aws_vpc.bench.id

  tags = { Name = "k0smotron-bench-igw" }
}

resource "aws_route_table" "bench" {
  vpc_id = aws_vpc.bench.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.bench.id
  }

  tags = { Name = "k0smotron-bench-rt" }
}

resource "aws_route_table_association" "bench" {
  subnet_id      = aws_subnet.bench.id
  route_table_id = aws_route_table.bench.id
}

###############################################################################
# Security groups
###############################################################################

# sg_internal — unrestricted traffic between all benchmark instances
resource "aws_security_group" "internal" {
  name        = "k0smotron-bench-internal"
  description = "Allow all traffic between benchmark instances (k0s cluster + storage)"
  vpc_id      = aws_vpc.bench.id

  ingress {
    description = "All traffic from members of this SG"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    self        = true
  }

  egress {
    description = "Allow all outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = { Name = "k0smotron-bench-internal" }
}

# sg_external — SSH + kubectl from the operator's IP/CIDR
resource "aws_security_group" "external" {
  name        = "k0smotron-bench-external"
  description = "Allow SSH (22) and kubectl (6443) from allowed_cidr"
  vpc_id      = aws_vpc.bench.id

  ingress {
    description = "SSH"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.allowed_cidr]
  }

  ingress {
    description = "kubectl / k0s API server"
    from_port   = 6443
    to_port     = 6443
    protocol    = "tcp"
    cidr_blocks = [var.allowed_cidr]
  }

  ingress {
    description = "HCP apiserver (NodePort) for perf test"
    from_port   = 30443
    to_port     = 30443
    protocol    = "tcp"
    cidr_blocks = [var.allowed_cidr]
  }

  egress {
    description = "Allow all outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = { Name = "k0smotron-bench-external" }
}

