###############################################################################
# Storage backend nodes — all in var.az (same AZ as management cluster)
###############################################################################

###############################################################################
# PostgreSQL node
#
# r6i.xlarge (4 vCPU, 32 GiB)
#   - Root: gp3 20 GB
#   - Data: io2 100 GB, 5000 IOPS
###############################################################################

resource "aws_instance" "postgres" {
  ami           = data.aws_ami.ubuntu.id
  instance_type = "r6i.xlarge"

  subnet_id         = aws_subnet.bench.id
  availability_zone = var.az
  key_name          = var.key_name
  # Storage bootstrap installs database packages from public apt repositories.
  # Access stays private because only the internal/storage security groups are attached.
  associate_public_ip_address = true

  vpc_security_group_ids = [
    aws_security_group.internal.id,
    aws_security_group.external.id,
  ]

  user_data = base64encode(templatefile("${path.module}/userdata/postgres.sh", {
    k0s_version    = var.k0s_version
    cp0_private_ip = aws_instance.cp_primary.private_ip
  }))
  user_data_replace_on_change = true

  root_block_device {
    volume_type           = "gp3"
    volume_size           = 20
    delete_on_termination = true
    encrypted             = true
    tags                  = merge(var.tags, { Name = "k0smotron-bench-postgres-root" })
  }

  ebs_block_device {
    device_name           = "/dev/sdb"
    volume_type           = "io2"
    volume_size           = 100
    iops                  = 5000
    delete_on_termination = true
    encrypted             = true
    tags                  = merge(var.tags, { Name = "k0smotron-bench-postgres-data" })
  }

  tags = merge(var.tags, {
    Name = "k0smotron-bench-postgres"
    Role = "storage-postgres"
  })

  lifecycle {
    ignore_changes = [ami]
  }
}

###############################################################################
# MySQL node
#
# r6i.xlarge (4 vCPU, 32 GiB)
#   - Root: gp3 20 GB
#   - Data: io2 100 GB, 5000 IOPS
###############################################################################

resource "aws_instance" "mysql" {
  ami           = data.aws_ami.ubuntu.id
  instance_type = "r6i.xlarge"

  subnet_id         = aws_subnet.bench.id
  availability_zone = var.az
  key_name          = var.key_name
  # Storage bootstrap installs database packages from public apt repositories.
  # Access stays private because only the internal/storage security groups are attached.
  associate_public_ip_address = true

  vpc_security_group_ids = [
    aws_security_group.internal.id,
    aws_security_group.external.id,
  ]

  user_data = base64encode(templatefile("${path.module}/userdata/mysql.sh", {
    k0s_version    = var.k0s_version
    cp0_private_ip = aws_instance.cp_primary.private_ip
  }))
  user_data_replace_on_change = true

  root_block_device {
    volume_type           = "gp3"
    volume_size           = 20
    delete_on_termination = true
    encrypted             = true
    tags                  = merge(var.tags, { Name = "k0smotron-bench-mysql-root" })
  }

  ebs_block_device {
    device_name           = "/dev/sdb"
    volume_type           = "io2"
    volume_size           = 100
    iops                  = 5000
    delete_on_termination = true
    encrypted             = true
    tags                  = merge(var.tags, { Name = "k0smotron-bench-mysql-data" })
  }

  tags = merge(var.tags, {
    Name = "k0smotron-bench-mysql"
    Role = "storage-mysql"
  })

  lifecycle {
    ignore_changes = [ami]
  }
}

###############################################################################
# MinIO node (used by t4 backends as the S3-compatible object store)
#
# r6i.large (2 vCPU, 16 GiB) — MinIO is light; the disk I/O profile matters
# more than the CPU. io2 100 GB / 5000 IOPS keeps the storage tier identical
# to postgres + mysql for fair comparison.
###############################################################################

resource "aws_instance" "minio" {
  ami           = data.aws_ami.ubuntu.id
  instance_type = "r6i.large"

  subnet_id                   = aws_subnet.bench.id
  availability_zone           = var.az
  key_name                    = var.key_name
  associate_public_ip_address = true

  vpc_security_group_ids = [
    aws_security_group.internal.id,
    aws_security_group.external.id,
  ]

  user_data = base64encode(templatefile("${path.module}/userdata/minio.sh", {
    k0s_version    = var.k0s_version
    cp0_private_ip = aws_instance.cp_primary.private_ip
  }))
  user_data_replace_on_change = true

  root_block_device {
    volume_type           = "gp3"
    volume_size           = 20
    delete_on_termination = true
    encrypted             = true
    tags                  = merge(var.tags, { Name = "k0smotron-bench-minio-root" })
  }

  ebs_block_device {
    device_name           = "/dev/sdb"
    volume_type           = "io2"
    volume_size           = 100
    iops                  = 5000
    delete_on_termination = true
    encrypted             = true
    tags                  = merge(var.tags, { Name = "k0smotron-bench-minio-data" })
  }

  tags = merge(var.tags, {
    Name = "k0smotron-bench-minio"
    Role = "storage-minio"
  })

  lifecycle {
    ignore_changes = [ami]
  }
}
